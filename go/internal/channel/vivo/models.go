package vivo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
)

// ID 是渠道标识。
const ID = "vivo"

const (
	// SandboxDomain 是沙箱环境，联调时把 API 的 baseURL 指向这里
	SandboxDomain = "https://sandbox-developer-api.vivo.com.cn/router/rest"
	// ReleaseDomain 是生产环境
	ReleaseDomain = "https://developer-api.vivo.com.cn/router/rest"

	successCode = "0"
	maxRaw      = 2000
)

// 业务方法名。vivo 是 router/rest 风格网关，所有接口共用一个路径，靠 method 区分。
const (
	methodGetAppInfo = "app.query.details"
	methodUploadAPK  = "app.upload.apk.app"
	methodSubmit     = "app.sync.update.app"
)

// envelope 是 vivo 接口响应的公共外层。
//
// Code / SubCode 用 any 而不是 int 或 string：vivo 在不同错误场景下这两个字段的
// JSON 类型并不稳定（限流走数字、部分鉴权失败走字符串）。声明成具体类型会在
// 反序列化阶段就失败，于是又把真实错误码顶掉了 —— 与上游 `get("subCode").asString`
// 抛 NPE 是同一类问题，只是换了个形式。
//
// 用 any 接住再自己归一化，目的是保证**无论渠道返回什么形状，
// 都先把错误码原样交到用户手里**。
type envelope struct {
	Code    any             `json:"code"`
	SubCode any             `json:"subCode"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

// codeText 把标量归一化成文本。
//
// encoding/json 把所有 JSON 数字解成 float64，直接格式化会得到 "20000" 之外的
// 情况如 "2e+04"，因此整数值要还原成整型文本。空串视为「渠道没返回」。
func (e envelope) codeText() string    { return scalarText(e.Code) }
func (e envelope) subCodeText() string { return scalarText(e.SubCode) }

func scalarText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

// ensureSuccess 判定业务成败。
//
// 两个关键决定：
//
//  1. 上游的 `get("subCode").asString.toIntOrNull()` 在错误响应（限流、鉴权失败）
//     往往只有 code 没有 subCode 时先抛 NPE，**把本该抛出的业务异常顶掉了**，
//     用户看到的是 NullPointerException 而不是真实错误码。这里两个码都空安全。
//
//  2. 「两个码都没有」不能当成成功 —— 那说明响应不是 vivo 的正常信封（接口变更、
//     网关返回了错误页、或者响应被截断）。把它判成成功会让 Submit 这种没有后续
//     data 校验的调用谎报发版成功，比抛 NPE 更危险。
func (e envelope) ensureSuccess(action, raw string) error {
	code, subCode := e.codeText(), e.subCodeText()

	if code == "" && subCode == "" {
		return &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg: fmt.Sprintf("%s 的响应里既无 code 也无 subCode，无法判定成败，"+
				"已按失败处理（接口可能已变更，或返回的不是 vivo 的正常响应）", action),
			Raw: truncate(raw),
		}
	}

	failed := (code != "" && code != successCode) || (subCode != "" && subCode != successCode)
	if !failed {
		return nil
	}

	reason := strings.TrimSpace(e.Msg)
	if reason == "" {
		reason = "渠道未返回错误描述"
	}
	// subCode 更具体，优先作为错误码上报
	reported := code
	if subCode != "" && subCode != successCode {
		reported = subCode
	}
	return &eperr.Error{
		Kind:    eperr.KindChannelRejected,
		Channel: ID,
		Code:    reported,
		Msg: fmt.Sprintf("%s 失败：%s（错误码含义见 "+
			"https://dev.vivo.com.cn/documentCenter/doc/330 ）", action, reason),
		Raw: truncate(raw),
	}
}

// apkResult 是上传接口的 data。
//
// 全部字段可空：上游在构造函数里链式取 `obj.get("x").asString`，任一 key 缺失即 NPE，
// 且一个字段缺失整个对象就构造不出来。这里校验推迟到 requireUploadResult，
// 以便给出带字段名的中文提示。
type apkResult struct {
	PackageName  string           `json:"packageName"`
	Serialnumber string           `json:"serialnumber"`
	VersionCode  *jsonx.FlexInt64 `json:"versionCode"`
	VersionName  string           `json:"versionName"`
	FileMd5      string           `json:"fileMd5"`
}

// UploadResult 是校验通过的上传结果，字段已确认非空。
type UploadResult struct {
	PackageName  string
	Serialnumber string
	VersionCode  int64
	FileMd5      string
}

// requireUploadResult 校验上传结果。
//
// 只硬性要求后续「提交更新」真正会用到的四个字段；versionName 不参与提交参数，
// 缺失不影响流程，因此不作为硬性要求。
// 缺字段说明接口协议变了，属于 ProtocolMismatch 而非业务拒绝。
func (r *apkResult) requireUploadResult(raw string) (UploadResult, error) {
	var missing []string
	if r == nil || strings.TrimSpace(r.PackageName) == "" {
		missing = append(missing, "packageName")
	}
	if r == nil || strings.TrimSpace(r.Serialnumber) == "" {
		missing = append(missing, "serialnumber")
	}
	if r == nil || r.VersionCode == nil {
		missing = append(missing, "versionCode")
	}
	if r == nil || strings.TrimSpace(r.FileMd5) == "" {
		missing = append(missing, "fileMd5")
	}
	if r == nil || len(missing) > 0 {
		label := strings.Join(missing, "、")
		if label == "" {
			label = "data"
		}
		return UploadResult{}, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "vivo 上传接口响应缺少必要字段：" + label,
			Raw:     truncate(raw),
		}
	}
	return UploadResult{
		PackageName:  r.PackageName,
		Serialnumber: r.Serialnumber,
		VersionCode:  int64(*r.VersionCode),
		FileMd5:      r.FileMd5,
	}, nil
}

// appInfo 是应用详情。审核状态字段在 vivo 文档里叫 status。
type appInfo struct {
	ReviewStatus *int             `json:"status"`
	VersionCode  *jsonx.FlexInt64 `json:"versionCode"`
	VersionName  string           `json:"versionName"`
}

// ToMarketInfo 转成渠道无关的状态。
//
// 版本信息缺失时 LastVersion 传 nil 而不是塞占位值：商店里只有尚未上传 APK 的
// 草稿时 vivo 不返回版本号，上游把 lastVersion 声明为非空，这种情况直接崩在解析阶段。
func (a appInfo) ToMarketInfo() channel.MarketInfo {
	var state channel.ReviewState
	if a.ReviewStatus != nil {
		switch *a.ReviewStatus {
		case 1:
			state = channel.ReviewDraft
		case 2:
			state = channel.ReviewUnderReview
		case 3:
			state = channel.ReviewOnline
		case 4:
			state = channel.ReviewRejected
		default:
			state = channel.ReviewUnknown
		}
	} else {
		state = channel.ReviewUnknown
	}

	var version *channel.Version
	if a.VersionCode != nil && strings.TrimSpace(a.VersionName) != "" {
		version = &channel.Version{Code: int64(*a.VersionCode), Name: a.VersionName}
	}

	raw := ""
	if a.ReviewStatus != nil {
		raw = strconv.Itoa(*a.ReviewStatus)
	}
	return channel.NewMarketInfo(ID, state, version, raw)
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}
