package publish

import (
	"context"
	"fmt"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// CheckStatus 是单项检查的结论。
type CheckStatus int

const (
	// CheckPass 通过
	CheckPass CheckStatus = iota
	// CheckWarn 需要人确认，但不阻断上架
	CheckWarn
	// CheckFail 不满足上架条件，必须先修
	CheckFail
	// CheckSkip 没能执行（缺配置、网络失败、该渠道不适用）
	CheckSkip
)

func (s CheckStatus) String() string {
	switch s {
	case CheckPass:
		return "通过"
	case CheckWarn:
		return "提醒"
	case CheckFail:
		return "不通过"
	case CheckSkip:
		return "跳过"
	}
	return "未知"
}

// Check 是一项上架前检查的结果。
type Check struct {
	// ID 是稳定标识，便于脚本按项判断
	ID string
	// Title 是一句话说明检查什么
	Title string
	// Status 是结论
	Status CheckStatus
	// Detail 是具体发现，通过时也可以有内容（如实际读到的应用名）
	Detail string
	// Fix 是失败时的修复建议
	Fix string
}

// ChecklistOptions 是执行检查的入参。
type ChecklistOptions struct {
	ApplicationID string
	ArtifactPath  string
	ChannelIDs    []string
	// ExpectLabel 覆盖配置里的 ExpectedLabel，用于临时核对
	ExpectLabel string
	Timeouts    httpx.Timeouts
}

// ChecklistResult 是一次完整检查的结果。
type ChecklistResult struct {
	ApplicationID string
	Artifact      artifact.Info
	Checks        []Check
}

// Failed 返回不通过的检查项。
func (r ChecklistResult) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c)
		}
	}
	return out
}

// Blocking 返回全部阻塞项（不通过 + 未能执行）。
//
// 未能执行的检查也算阻塞：跳过一项检查不等于那一项没问题，
// 把「查不了」当成「没问题」是这个命令最容易犯的错。
func (r ChecklistResult) Blocking() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckFail || c.Status == CheckSkip {
			out = append(out, c)
		}
	}
	return out
}

// Checklist 执行上架前检查。
//
// 设计取舍：**不做自动修复**，只报告。上架前的每一项都涉及对外可见的素材或
// 不可撤销的动作，自动改配置或自动改包都可能把问题藏起来。这个命令的职责是
// 让「以为什么都对了」变成「逐项确认过」。
//
// 检查分两类：制品本身的（本地即可判定）与渠道状态的（要联网）。
// 后者失败不影响前者 —— 逐项给出结论，不因为一个渠道超时就整份报告作废。
func (s *Service) Checklist(
	ctx context.Context,
	opts ChecklistOptions,
) (ChecklistResult, error) {
	cfg, err := s.store.Require(opts.ApplicationID)
	if err != nil {
		return ChecklistResult{}, err
	}

	info, err := s.readInfo(opts.ArtifactPath)
	if err != nil {
		// 制品读不出来就没法继续后面的检查，但仍然返回一份可读的报告，
		// 而不是一个光秃秃的解析错误
		return ChecklistResult{
			ApplicationID: cfg.ApplicationID,
			Checks: []Check{{
				ID:     "artifact-readable",
				Title:  "制品可解析",
				Status: CheckFail,
				Detail: err.Error(),
				Fix:    "确认路径指向构建产物（release 包，非 debug 包），且文件完整",
			}},
		}, nil
	}

	result := ChecklistResult{ApplicationID: cfg.ApplicationID, Artifact: info}
	result.Checks = append(result.Checks, artifactChecks(cfg, info, opts.ExpectLabel)...)
	result.Checks = append(result.Checks, s.channelChecks(ctx, cfg, opts, info)...)
	return result, nil
}

// artifactChecks 是制品本身与配置的一致性检查（不联网）。
func artifactChecks(
	cfg config.AppConfig,
	info artifact.Info,
	expectLabel string,
) []Check {
	checks := []Check{{
		ID:     "artifact-readable",
		Title:  "制品可解析",
		Status: CheckPass,
		Detail: info.String(),
	}}

	// 包名必须与配置一致：传错包是最容易发生、后果又最严重的一类失误
	if !strings.EqualFold(info.ApplicationID, cfg.ApplicationID) {
		checks = append(checks, Check{
			ID:     "package-match",
			Title:  "包名与配置一致",
			Status: CheckFail,
			Detail: fmt.Sprintf("制品是 %s，配置的应用是 %s",
				info.ApplicationID, cfg.ApplicationID),
			Fix: "确认 --artifact 指向的是这个应用安装包，或纠正 --app 的包名",
		})
	} else {
		checks = append(checks, Check{
			ID:     "package-match",
			Title:  "包名与配置一致",
			Status: CheckPass,
			Detail: info.ApplicationID,
		})
	}

	checks = append(checks, apkLabelCheck(cfg, info, expectLabel))
	checks = append(checks, apkIconCheck(info))
	return checks
}

// apkLabelCheck 比对 APK 内的应用名。
//
// 这是渠道实际驳回过的问题：android:label 停留在脚手架占位符（"civilian"），
// 而商店页展示的是中文名，华为以 "AppName is not same as it in apk package"
// 拒绝送审。
func apkLabelCheck(cfg config.AppConfig, info artifact.Info, expectLabel string) Check {
	const (
		id    = "app-label"
		title = "应用名与商店一致"
	)
	if info.Kind != artifact.KindAPK {
		return Check{
			ID: id, Title: title, Status: CheckSkip,
			Detail: "鸿蒙的应用名与图标由 AGC 后台素材决定，制品内不含该信息",
		}
	}

	detail, err := artifact.ReadManifestDetail(info.Path)
	if err != nil {
		return Check{
			ID: id, Title: title, Status: CheckSkip,
			Detail: fmt.Sprintf("读取 AndroidManifest 失败：%v", err),
			Fix:    "确认这是可正常安装的 APK",
		}
	}

	expected := strings.TrimSpace(expectLabel)
	if expected == "" {
		expected = strings.TrimSpace(cfg.ExpectedLabel)
	}

	if detail.LabelIsPlaceholder() {
		return Check{
			ID: id, Title: title, Status: CheckFail,
			Detail: fmt.Sprintf("应用名是「%s」，看着像脚手架占位符", detail.Label),
			Fix: "改 AndroidManifest.xml 的 android:label 为线上应用名，" +
				"或改用 @string/app_name 并在 strings.xml 里填中文名",
		}
	}

	if expected == "" {
		return Check{
			ID: id, Title: title, Status: CheckWarn,
			Detail: fmt.Sprintf("制品内的应用名是「%s」；未配置期望名，无法自动比对",
				detail.Label),
			Fix: "用 `app add --label` 或 `checklist --expect-label` 提供商店页的应用名，" +
				"之后即可自动比对",
		}
	}

	if !detail.AppLabelMismatch(expected) {
		return Check{
			ID: id, Title: title, Status: CheckPass,
			Detail: fmt.Sprintf("「%s」", detail.Label),
		}
	}
	return Check{
		ID: id, Title: title, Status: CheckFail,
		Detail: fmt.Sprintf("制品内是「%s」，商店页是「%s」", detail.Label, expected),
		Fix: "两者必须逐字一致。改 AndroidManifest.xml 的 android:label，" +
			"或到各渠道后台改商店素材",
	}
}

// apkIconCheck 检查图标是否还是脚手架模板图。
//
// 同样是渠道驳回过的问题：mipmap 里留着 Flutter 默认的灰底图标，
// 而商店页是真图标，被判「图标与应用安装后不一致」。
func apkIconCheck(info artifact.Info) Check {
	const (
		id    = "app-icon"
		title = "图标不是模板图"
	)
	if info.Kind != artifact.KindAPK {
		return Check{
			ID: id, Title: title, Status: CheckSkip,
			Detail: "鸿蒙的图标由 AGC 后台素材决定",
		}
	}

	detail, err := artifact.ReadManifestDetail(info.Path)
	if err != nil {
		return Check{
			ID: id, Title: title, Status: CheckSkip,
			Detail: fmt.Sprintf("读取 AndroidManifest 失败：%v", err),
		}
	}

	icon := artifact.CheckIcon(info.Path, detail.IconPath)
	switch {
	case icon.IsTemplate:
		return Check{
			ID: id, Title: title, Status: CheckFail,
			Detail: fmt.Sprintf("android:icon 命中了脚手架默认图标（%s）", icon.Match),
			Fix: "把 android/app/src/main/res/mipmap-*/ic_launcher.png 换成实际图标，" +
				"各密度都要换",
		}
	case icon.Checked:
		return Check{
			ID: id, Title: title, Status: CheckPass,
			Detail: "不是已知的脚手架模板图标",
		}
	default:
		return Check{
			ID: id, Title: title, Status: CheckWarn,
			Detail: icon.Detail,
			Fix:    "人工确认应用图标与商店页一致",
		}
	}
}

// channelChecks 是各渠道的线上状态与版本号检查。
func (s *Service) channelChecks(
	ctx context.Context,
	cfg config.AppConfig,
	opts ChecklistOptions,
	info artifact.Info,
) []Check {
	states, err := s.MarketStates(ctx, cfg.ApplicationID, opts.ChannelIDs, opts.Timeouts)
	if err != nil {
		return []Check{{
			ID: "channel-query", Title: "渠道状态可查询", Status: CheckSkip,
			Detail: err.Error(),
			Fix:    "检查网络与凭据配置后重试；无法确认渠道状态时不要直接送审",
		}}
	}

	// 顺序取注入的渠道列表，而不是全局注册表 channel.All()：
	// 测试注入的替身不在注册表里，用注册表过滤会把它们全丢掉。
	// 真实运行时两者一致（NewService 默认就取 channel.All()）。
	order := make([]string, 0, len(states))
	for _, ch := range s.channels {
		if _, ok := states[ch.ID()]; ok {
			order = append(order, ch.ID())
		}
	}

	checks := make([]Check, 0, len(order)*2)
	for _, id := range order {
		res := states[id]
		name := id
		if ch, ok := channel.Find(id); ok {
			name = ch.DisplayName()
		}

		if res.Err != nil {
			checks = append(checks, Check{
				ID: "channel-state-" + id, Title: name + "：状态可查询",
				Status: CheckSkip,
				Detail: res.Err.Error(),
				Fix:    "先修好这个渠道的查询再上架；状态未知时无法判断能否提交",
			})
			continue
		}

		state := res.Info.ReviewState
		switch state {
		case channel.ReviewUnderReview:
			checks = append(checks, Check{
				ID: "channel-state-" + id, Title: name + "：当前不在审核中",
				Status: CheckFail, Detail: "该渠道正在审核，新版本会被拒绝",
				Fix: "等这一轮审核结束，或本次跳过该渠道（--channel 排除它）",
			})
			continue
		case channel.ReviewRejected:
			// 被拒不是阻塞：重新提交正是为了覆盖上一次的驳回
			checks = append(checks, Check{
				ID: "channel-state-" + id, Title: name + "：当前不在审核中",
				Status: CheckWarn, Detail: "该渠道上一次审核被拒，本次提交是对驳回的覆盖",
				Fix: "确认本次已修复驳回意见中列出的问题",
			})
		case channel.ReviewUnknown:
			// 未知状态**不能判通过**。CanSubmit 是按「不在审核中」推导的，
			// 未知状态于是得出 true —— 但那是缺省，不是渠道的确认。
			// 渠道返回了我们不认识的状态码时，真实情况可能是被拒、可能是在审核。
			checks = append(checks, Check{
				ID: "channel-state-" + id, Title: name + "：当前不在审核中",
				Status: CheckWarn,
				Detail: fmt.Sprintf("渠道返回了未识别的状态值 %q，无法确认它是否在审核中",
					res.Info.RawState),
				Fix: "登录该渠道开发者后台确认状态；确认不在审核中再提交",
			})
		default:
			checks = append(checks, Check{
				ID: "channel-state-" + id, Title: name + "：当前不在审核中",
				Status: CheckPass, Detail: state.Label(),
			})
		}

		checks = append(checks, versionCheck(name, id, res.Info, info))
	}
	return checks
}

// versionCheck 比对本制品与渠道线上版本号。
//
// 这里直接比较版本号而不是复用 Reject：Reject 是「这次能不能提交」的综合判断
// （还含渠道状态、策略档位），而 checklist 要的是「版本号这一项是否过关」，
// 结论要写进报告里给人看，用比较结果生成说明更清楚。
func versionCheck(name, id string, market channel.MarketInfo, info artifact.Info) Check {
	check := Check{ID: "version-" + id, Title: name + "：版本号高于线上"}

	online := market.LastVersion
	if online == nil {
		// 线上没有可比对的版本 —— 首次上架，或只有未上传包的草稿版本。
		// 后者是渠道的正常行为，不是错误
		check.Status = CheckPass
		check.Detail = "线上暂无版本号可比对（首次上架，或只有未上传包的草稿）"
		return check
	}

	check.Detail = fmt.Sprintf("本制品 %d，线上 %d（%s）",
		info.VersionCode, online.Code, online.Name)

	switch {
	case info.VersionCode > online.Code:
		check.Status = CheckPass

	case info.VersionCode == online.Code:
		// 同号只在「仅换素材」时合法，且必须显式开 --allow-same-version，
		// 否则渠道自己会拒。这里不能判成失败 —— 那种场景本来就存在
		check.Status = CheckWarn
		check.Detail += "；同号提交会被渠道拒绝，除非显式加 --allow-same-version"
		check.Fix = "只改了素材没改代码时才用 --allow-same-version；" +
			"否则把版本号往上加一位再构建"

	default:
		check.Status = CheckFail
		check.Detail += "；低于线上版本"
		check.Fix = "版本号必须严格递增（pubspec.yaml 的 +buildNumber 或 " +
			"build.gradle 的 versionCode），往上加后重新构建"
	}
	return check
}
