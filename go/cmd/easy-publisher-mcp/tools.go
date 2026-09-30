package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools 注册全部工具。
//
// 安全约束（写进实现，不只写文档）：
//   - **没有任何工具接受凭据参数。** 凭据由 core 从 ~/.easy-publisher/ 或环境变量
//     读取，绝不经过模型的上下文。
//   - 只读工具与写工具通过 ToolAnnotations 区分，写工具标注为非幂等且有副作用。
//   - upload_apk 需要显式确认，理由见该工具的说明。
func registerTools(server *mcp.Server, svc *publish.Service) {
	registerListApps(server, svc)
	registerListChannels(server)
	registerGetMarketState(server, svc)
	registerChecklist(server, svc)
	registerCheckRelease(server, svc)
	registerUploadApk(server, svc)
	registerGetUploadStatus(server, svc)
}

// ---- list_apps ----

type listAppsInput struct{}

type appSummary struct {
	ApplicationID   string           `json:"applicationId"`
	Name            string           `json:"name"`
	MultiChannelApk bool             `json:"multiChannelApk"`
	Channels        []channelSummary `json:"channels"`
}

type channelSummary struct {
	ID               string   `json:"id"`
	Enabled          bool     `json:"enabled"`
	ConfiguredParams []string `json:"configuredParams"`
}

type listAppsOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error *errorPayload `json:"error,omitempty"`
	Apps  []appSummary  `json:"apps"`
}

func registerListApps(server *mcp.Server, svc *publish.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_apps",
		Description: "列出已配置的应用及其启用的渠道。只返回参数名，不返回凭据值。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listAppsInput) (*mcp.CallToolResult, listAppsOutput, error) {
		apps, err := svc.ListApps()
		if err != nil {
			return errorResult(err, func(e *errorPayload) listAppsOutput { return listAppsOutput{Error: e} })
		}
		out := listAppsOutput{Apps: []appSummary{}}
		for _, app := range apps {
			item := appSummary{
				ApplicationID:   app.ApplicationID,
				Name:            app.Name,
				MultiChannelApk: app.MultiChannelApk,
				Channels:        []channelSummary{},
			}
			for _, ch := range app.Channels {
				// 只报参数名，绝不报参数值
				names := make([]string, 0, len(ch.Params))
				for _, p := range ch.Params {
					names = append(names, p.Name)
				}
				item.Channels = append(item.Channels, channelSummary{
					ID: ch.Name, Enabled: ch.Enabled, ConfiguredParams: names,
				})
			}
			out.Apps = append(out.Apps, item)
		}
		return nil, out, nil
	})
}

// ---- list_channels ----

type listChannelsInput struct{}

type capabilitySummary struct {
	SupportedStages               []string `json:"supportedStages"`
	RiskLevel                     string   `json:"riskLevel"`
	RiskLevelLabel                string   `json:"riskLevelLabel"`
	Withdrawal                    string   `json:"withdrawal"`
	WithdrawalLabel               string   `json:"withdrawalLabel"`
	RequiresExplicitConfirmation  bool     `json:"requiresExplicitConfirmation"`
	AutomaticRetryAfterSubmission bool     `json:"automaticRetryAfterSubmission"`
	Evidence                      string   `json:"evidence"`
	EvidenceLabel                 string   `json:"evidenceLabel"`
	// VerifiedScope 仅在已实测时有值。它写明了哪些部分**没有**验证 ——
	// 「已实测」不含范围会被读成整条链路都验证过
	VerifiedScope string `json:"verifiedScope,omitempty"`
	Note          string `json:"note"`
}

type channelInfo struct {
	ID          string             `json:"id"`
	DisplayName string             `json:"displayName"`
	FileNameTag string             `json:"fileNameTag"`
	Capability  capabilitySummary  `json:"capability"`
	Params      []channelParamInfo `json:"params"`
}

type channelParamInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Type        string `json:"type"`
	EnvName     string `json:"envName"`
}

type listChannelsOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error    *errorPayload `json:"error,omitempty"`
	Channels []channelInfo `json:"channels"`
}

func registerListChannels(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_channels",
		Description: "列出支持的应用商店渠道、各渠道所需凭据参数、以及发布能力矩阵。" +
			"用于了解需要配置什么，以及某个渠道能停在流程的哪一步。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listChannelsInput) (*mcp.CallToolResult, listChannelsOutput, error) {
		out := listChannelsOutput{Channels: []channelInfo{}}
		for _, ch := range channel.All() {
			caps := ch.Capabilities()
			item := channelInfo{
				ID: ch.ID(), DisplayName: ch.DisplayName(),
				FileNameTag: ch.FileNameTag(),
				Capability: capabilitySummary{
					RiskLevel:                     caps.RiskLevel.String(),
					RiskLevelLabel:                caps.RiskLevel.Label(),
					Withdrawal:                    caps.Withdrawal.String(),
					WithdrawalLabel:               caps.Withdrawal.Label(),
					RequiresExplicitConfirmation:  caps.RequiresExplicitConfirmation,
					AutomaticRetryAfterSubmission: caps.AutomaticRetryAfterSubmission,
					Evidence:                      caps.Evidence.String(),
					EvidenceLabel:                 caps.Evidence.Label(),
					VerifiedScope:                 caps.VerifiedScope,
					Note:                          caps.Note,
				},
				Params: []channelParamInfo{},
			}
			for _, s := range caps.SupportedStages {
				item.Capability.SupportedStages = append(item.Capability.SupportedStages, s.String())
			}
			for _, p := range ch.Params() {
				typeName := "text"
				if p.Type == channel.ParamTextFile {
					typeName = "file(." + p.FileExtension + ")"
				}
				item.Params = append(item.Params, channelParamInfo{
					Name: p.Name, Description: p.Description,
					Required: p.Required, Type: typeName,
					EnvName: config.EnvName(ch.ID(), p.Name),
				})
			}
			out.Channels = append(out.Channels, item)
		}
		return nil, out, nil
	})
}

// ---- get_market_state ----

type marketStateInput struct {
	ApplicationID string   `json:"applicationId" jsonschema:"包名，如 com.example.app"`
	Channels      []string `json:"channels,omitempty" jsonschema:"只查指定渠道；省略则查全部已启用渠道"`
	TimeoutSec    int64    `json:"timeoutSeconds,omitempty" jsonschema:"单次请求超时秒数，默认 120"`
}

type reviewNoteJSON struct {
	Kind    string `json:"kind"`
	Passed  *bool  `json:"passed,omitempty"`
	Opinion string `json:"opinion,omitempty"`
}

type marketStateItem struct {
	ID               string `json:"id"`
	OK               bool   `json:"ok"`
	ReviewState      string `json:"reviewState,omitempty"`
	ReviewStateLabel string `json:"reviewStateLabel,omitempty"`
	RawStateLabel    string `json:"rawStateLabel,omitempty"`
	CanSubmit        *bool  `json:"canSubmit,omitempty"`
	LastVersionCode  *int64 `json:"lastVersionCode,omitempty"`
	LastVersionName  string `json:"lastVersionName,omitempty"`
	RawState         string `json:"rawState,omitempty"`
	// RejectReason 是渠道给出的审核意见原文。
	//
	// 各渠道都没有结构化的原因码，只有自由文本 —— 因此原样转述，
	// 由调用方（通常是模型）去读，而不是我们做分类或归纳。
	RejectReason string `json:"rejectReason,omitempty"`
	// RejectAttachments 是审核意见附件的 URL。审核员截的图往往比文字说明更具体
	RejectAttachments []string `json:"rejectAttachments,omitempty"`
	// ReviewNotes 是渠道按维度给出的补充意见（华为的版权/版号/备案）
	ReviewNotes []reviewNoteJSON `json:"reviewNotes,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	Message     string           `json:"message,omitempty"`
	Retryable   *bool            `json:"retryable,omitempty"`
}

type marketStateOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error         *errorPayload     `json:"error,omitempty"`
	ApplicationID string            `json:"applicationId"`
	Channels      []marketStateItem `json:"channels"`
}

func registerGetMarketState(server *mcp.Server, svc *publish.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_market_state",
		Description: "查询应用在各渠道的审核状态与线上版本号。发布前应先调用此工具：" +
			"若某渠道正在审核中，提交新版本会被拒绝。" +
			"lastVersionCode 可能缺失（应用在商店只有未上传安装包的草稿版本时，渠道不返回版本号）。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in marketStateInput) (*mcp.CallToolResult, marketStateOutput, error) {
		applicationID, err := artifact.ValidateApplicationID(in.ApplicationID)
		if err != nil {
			return errorResult(err, func(e *errorPayload) marketStateOutput { return marketStateOutput{Error: e} })
		}
		results, err := svc.MarketStates(ctx, applicationID, in.Channels, timeoutsOf(in.TimeoutSec))
		if err != nil {
			return errorResult(err, func(e *errorPayload) marketStateOutput { return marketStateOutput{Error: e} })
		}

		out := marketStateOutput{ApplicationID: applicationID, Channels: []marketStateItem{}}
		// 按注册顺序输出，结果才稳定
		for _, ch := range channel.All() {
			res, ok := results[ch.ID()]
			if !ok {
				continue
			}
			item := marketStateItem{ID: ch.ID()}
			if res.Err != nil {
				item.OK = false
				item.Message = res.Err.Error()
				var pe *eperr.Error
				if errors.As(res.Err, &pe) {
					item.Kind = pe.Kind.String()
					r := pe.Retryable()
					item.Retryable = &r
				}
			} else {
				item.OK = true
				item.ReviewState = res.Info.ReviewState.String()
				item.ReviewStateLabel = res.Info.ReviewState.Label()
				can := res.Info.CanSubmit
				item.CanSubmit = &can
				if v := res.Info.LastVersion; v != nil {
					code := v.Code
					item.LastVersionCode = &code
					item.LastVersionName = v.Name
				}
				item.RawState = res.Info.RawState
				item.RawStateLabel = res.Info.RawStateLabel
			}
			// 审核意见：各渠道都只有自由文本，没有结构化原因码 ——
			// 原样带出去，让调用方自己读，我们不做分类或归纳
			if r := res.Info.Review; r != nil {
				item.RejectReason = r.Opinion
				item.RejectAttachments = r.Attachments
				for _, n := range r.Notes {
					item.ReviewNotes = append(item.ReviewNotes, reviewNoteJSON{
						Kind: n.Kind, Passed: n.Passed, Opinion: n.Opinion,
					})
				}
			}
			out.Channels = append(out.Channels, item)
		}
		return nil, out, nil
	})
}

// ---- checklist ----

type checklistInput struct {
	ApplicationID string   `json:"applicationId" jsonschema:"包名"`
	ArtifactPath  string   `json:"artifactPath" jsonschema:"待上架的制品路径（.apk，鸿蒙为 .app）"`
	Channels      []string `json:"channels,omitempty" jsonschema:"只检查指定渠道；省略则检查全部已启用渠道"`
	ExpectLabel   string   `json:"expectLabel,omitempty" jsonschema:"商店页展示的应用名，用于比对 APK 内的 android:label"`
	TimeoutSec    int64    `json:"timeoutSeconds,omitempty" jsonschema:"单次请求超时秒数，默认 120"`
}

type checklistOutput struct {
	// Error 非空表示本次调用失败
	Error         *errorPayload    `json:"error,omitempty"`
	Artifact      *artifactSummary `json:"artifact,omitempty"`
	Checks        []checklistItem  `json:"checks"`
	BlockedCount  int              `json:"blockedCount"`
	FailedCount   int              `json:"failedCount"`
	Ready         bool             `json:"ready"`
	ApplicationID string           `json:"applicationId"`
}

// checklist 是上架前的逐项检查。
//
// 与 check_release 的分工：check_release 面向「这次提交会不会被渠道拒绝」，
// 逐渠道给结论；checklist 面向「这个包本身有没有低级错误」，逐项给结论，
// 且把「查不了」也算作阻塞。上架前建议两个都跑 —— 它们是不同的失败面。
func registerChecklist(server *mcp.Server, svc *publish.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "checklist",
		Description: "上架前逐项检查（只读，不会提交任何东西）。检查制品本身的问题：" +
			"包名与配置是否一致、应用名是否还是脚手架占位符、图标是否还是模板图、" +
			"版本号是否高于各渠道线上版本；同时检查各渠道是否正在审核中。" +
			"每一项都对应一类真实发生过的驳回。" +
			"status 为「不通过」或「跳过」的都算阻塞项（跳过 = 没能检查，不等于没问题）。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checklistInput) (*mcp.CallToolResult, checklistOutput, error) {
		applicationID, err := artifact.ValidateApplicationID(in.ApplicationID)
		if err != nil {
			return errorResult(err, func(e *errorPayload) checklistOutput { return checklistOutput{Error: e} })
		}

		result, err := svc.Checklist(ctx, publish.ChecklistOptions{
			ApplicationID: applicationID,
			ArtifactPath:  in.ArtifactPath,
			ChannelIDs:    in.Channels,
			ExpectLabel:   in.ExpectLabel,
			Timeouts:      timeoutsOf(in.TimeoutSec),
		})
		if err != nil {
			return errorResult(err, func(e *errorPayload) checklistOutput { return checklistOutput{Error: e} })
		}

		out := checklistOutput{
			ApplicationID: result.ApplicationID,
			Checks:        []checklistItem{},
			BlockedCount:  len(result.Blocking()),
			FailedCount:   len(result.Failed()),
		}
		out.Ready = out.BlockedCount == 0
		if result.Artifact.Path != "" {
			a := result.Artifact
			out.Artifact = &artifactSummary{
				Path: a.Path, ApplicationID: a.ApplicationID,
				VersionCode: a.VersionCode, VersionName: a.VersionName,
				SizeBytes: a.SizeBytes,
			}
		}
		for _, c := range result.Checks {
			out.Checks = append(out.Checks, checklistItem{
				ID: c.ID, Title: c.Title, Status: c.Status.String(),
				Detail: c.Detail, Fix: c.Fix,
			})
		}
		return nil, out, nil
	})
}

// ---- check_release ----

type checkReleaseInput struct {
	ApplicationID    string   `json:"applicationId" jsonschema:"包名"`
	ArtifactPath     string   `json:"artifactPath" jsonschema:"制品文件路径（.apk，鸿蒙为 .app），或存放多渠道包的目录"`
	Channels         []string `json:"channels,omitempty" jsonschema:"只检查指定渠道；省略则检查全部已启用渠道"`
	AllowSameVersion bool     `json:"allowSameVersion,omitempty" jsonschema:"允许版本号与线上相同"`
	ExpectLabel      string   `json:"expectLabel,omitempty" jsonschema:"商店页展示的应用名，用于比对 APK 内的 android:label。省略则用应用配置里的 expectedLabel"`
}

// checklistItem 是一项上架前检查的结论。
type checklistItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type checkReleaseItem struct {
	ID               string `json:"id"`
	CanRelease       bool   `json:"canRelease"`
	ReviewStateLabel string `json:"reviewStateLabel,omitempty"`
	// RawStateLabel 是渠道文档里该状态值的原始描述（如「撤销上架」）。
	// ReviewStateLabel 是粗分类，这个是渠道自己的措辞
	RawStateLabel    string `json:"rawStateLabel,omitempty"`
	LastVersionCode  *int64 `json:"lastVersionCode,omitempty"`
	BlockedReason    string `json:"blockedReason,omitempty"`
	StateQueryFailed string `json:"stateQueryFailed,omitempty"`
}

type checkReleaseOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error    *errorPayload    `json:"error,omitempty"`
	Artifact *artifactSummary `json:"artifact,omitempty"`
	Warning  string           `json:"warning,omitempty"`
	Note     string           `json:"note,omitempty"`
	// Checks 是制品级的检查项（应用名、图标等）。
	// 结论为「跳过」的项同样算阻塞 —— 查不了不等于没问题。
	Checks         []checklistItem    `json:"checks,omitempty"`
	Channels       []checkReleaseItem `json:"channels"`
	BlockedCount   int                `json:"blockedCount"`
	ResolvedStages map[string]string  `json:"resolvedStages"`
}

type artifactSummary struct {
	Path          string `json:"path"`
	ApplicationID string `json:"applicationId"`
	VersionCode   int64  `json:"versionCode"`
	VersionName   string `json:"versionName"`
	SizeBytes     int64  `json:"sizeBytes"`
}

// check_release 是发布前的只读预检。
//
// 存在的理由：upload_apk 是不可撤销的操作，模型应当有一个**零副作用**的方式
// 先确认「这个包发到这些渠道会不会被拒」。没有这个工具，模型只能靠真发一次来试。
func registerCheckRelease(server *mcp.Server, svc *publish.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "check_release",
		Description: "发布预检（只读，不会提交任何东西）。解析制品、检查应用名与图标是否合规、" +
			"查询各渠道状态，逐渠道判断是否满足发布前置条件，并说明不满足的原因。" +
			"checks 里 status 为「不通过」或「跳过」的都算阻塞项，需先处理。" +
			"建议在 upload_apk 之前调用。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkReleaseInput) (*mcp.CallToolResult, checkReleaseOutput, error) {
		applicationID, err := artifact.ValidateApplicationID(in.ApplicationID)
		if err != nil {
			return errorResult(err, func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
		}

		rule := publish.VersionStrict
		if in.AllowSameVersion {
			rule = publish.VersionAllowSame
		}

		out := checkReleaseOutput{Channels: []checkReleaseItem{}}

		st, statErr := os.Stat(in.ArtifactPath)
		var info *artifact.Info
		switch {
		case statErr != nil:
			return errorResult(eperr.LocalFileError(
				"路径不存在：%s", in.ArtifactPath), func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
		case !st.IsDir():
			parsed, err := artifact.Read(in.ArtifactPath)
			if err != nil {
				return errorResult(err, func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
			}
			info = &parsed
			out.Artifact = &artifactSummary{
				Path: parsed.Path, ApplicationID: parsed.ApplicationID,
				VersionCode: parsed.VersionCode, VersionName: parsed.VersionName,
				SizeBytes: parsed.SizeBytes,
			}
			if parsed.ApplicationID != applicationID {
				out.Warning = fmt.Sprintf(
					"制品的包名 %s 与配置的 %s 不一致", parsed.ApplicationID, applicationID)
			}
		default:
			out.Note = "artifactPath 是目录，将按渠道标识匹配多渠道包，此处不逐个解析"
		}

		states, err := svc.MarketStates(ctx, applicationID, in.Channels, httpx.Default())
		if err != nil {
			return errorResult(err, func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
		}
		stages, err := svc.ResolvedStages(applicationID, in.Channels, nil)
		if err != nil {
			return errorResult(err, func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
		}
		out.ResolvedStages = map[string]string{}
		for id, s := range stages {
			out.ResolvedStages[id] = s.String()
		}

		// 制品级检查：应用名、图标、包名一致性。
		// 这些检查不联网，即使渠道状态查询失败也能给出结论
		if info != nil {
			cfg, cfgErr := svc.App(applicationID)
			if cfgErr != nil {
				return errorResult(cfgErr, func(e *errorPayload) checkReleaseOutput { return checkReleaseOutput{Error: e} })
			}
			for _, c := range publish.ArtifactChecks(cfg, *info, in.ExpectLabel) {
				out.Checks = append(out.Checks, checklistItem{
					ID: c.ID, Title: c.Title, Status: c.Status.String(),
					Detail: c.Detail, Fix: c.Fix,
				})
				if c.Status == publish.CheckFail || c.Status == publish.CheckSkip {
					out.BlockedCount++
				}
			}
		}

		for _, ch := range channel.All() {
			res, ok := states[ch.ID()]
			if !ok {
				continue
			}
			item := checkReleaseItem{ID: ch.ID()}

			var market *channel.MarketInfo
			if res.Err == nil {
				market = &res.Info
				item.ReviewStateLabel = res.Info.ReviewState.Label()
				item.RawStateLabel = res.Info.RawStateLabel
				if v := res.Info.LastVersion; v != nil {
					code := v.Code
					item.LastVersionCode = &code
				}
			} else {
				// 状态查询失败不阻止预检的其余部分：
				// 鸿蒙渠道就是有意不支持状态查询的
				item.StateQueryFailed = res.Err.Error()
			}

			if info != nil {
				if rejection := publish.Reject(*info, market, rule); rejection != nil {
					item.BlockedReason = rejection.Msg
				}
			}
			item.CanRelease = item.BlockedReason == "" && res.Err == nil
			if !item.CanRelease {
				out.BlockedCount++
			}
			out.Channels = append(out.Channels, item)
		}
		return nil, out, nil
	})
}

// ---- upload_apk ----

type uploadInput struct {
	ApplicationID    string   `json:"applicationId" jsonschema:"包名"`
	ArtifactPath     string   `json:"artifactPath" jsonschema:"制品文件路径（.apk，鸿蒙为 .app），或存放多渠道包的目录"`
	UpdateDesc       string   `json:"updateDesc" jsonschema:"更新说明"`
	Confirm          bool     `json:"confirm,omitempty" jsonschema:"当本次操作包含送审时必须为 true。确认理解送审不可撤销。若所有目标渠道都只走到 artifact 或 draft，则不需要此参数"`
	Channels         []string `json:"channels,omitempty" jsonschema:"只发指定渠道；省略则发全部已启用渠道"`
	OnlineTime       string   `json:"onlineTime,omitempty" jsonschema:"定时上线时间，格式 yyyy-MM-dd HH:mm:ss；省略则审核通过后立即发布"`
	AllowSameVersion bool     `json:"allowSameVersion,omitempty" jsonschema:"允许版本号与线上相同（审核被拒后仅更新素材时使用）。版本号低于线上仍会被拒绝"`
	// 取值见 Tool.Description。
	//
	// 这里不能写 "artifact=..." 那种枚举说明：SDK 会解析 jsonschema tag，
	// 把 WORD= 形式当成 tag 关键字并 panic
	StopAfter  string `json:"stopAfter,omitempty" jsonschema:"流程走到哪一步就停下。取值与限制见工具说明"`
	TimeoutSec int64  `json:"timeoutSeconds,omitempty" jsonschema:"单次请求超时秒数，默认 120"`
}

type uploadOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error          *errorPayload `json:"error,omitempty"`
	JobID          string        `json:"jobId"`
	RequestedStage string        `json:"requestedStage,omitempty"`
	// omitempty 不可省：fail 路径返回的是零值，nil map 会序列化成 null，
	// 而 SDK 在错误路径上同样校验输出结构，null 与 schema 的 object 冲突
	TargetStages map[string]string `json:"targetStages,omitempty"`
	Channels     []string          `json:"channels,omitempty"`
	Note         string            `json:"note"`
}

// upload_apk 提交新版本。
//
// # 为什么需要 confirm
//
// 各应用商店的 API **都不提供撤销版本更新的接口**，提交即不可逆。CLI 场景下
// 有人类在键盘前敲命令，误触的代价有限；但作为 MCP 工具，调用方是自主决策的模型，
// 一次误判就会把一个未经审阅的包推到正式渠道，且无法回滚。
//
// 因此这里要求显式传 confirm。这不是防御恶意调用（模型完全可以传 true），
// 而是把「这一步不可逆」变成 schema 层面必须正视的事实，而不是藏在 description 里
// 的一句提醒。
//
// 反过来说，停在送审之前不产生不可撤销的副作用，强行要求 confirm 只会训练调用方
// 无脑传 true，反而削弱这道门槛 —— 所以只有本次真的包含送审时才要求。
func registerUploadApk(server *mcp.Server, svc *publish.Service) {
	destructive := true
	openWorld := true
	mcp.AddTool(server, &mcp.Tool{
		Name: "upload_apk",
		Description: "上传制品并向应用商店提交新版本。\n\n" +
			"警告：此操作不可撤销 —— 各应用商店均未提供撤销版本更新的 API。\n" +
			"提交后若要停止发布，只能登录各商店后台手动操作。\n\n" +
			"必须显式传 confirm=true 才会执行送审。建议先调用 check_release 预检。\n" +
			"本工具立即返回 jobId，不等待上传完成；用 get_upload_status 轮询进度。\n\n" +
			"stopAfter 可取三个值：\n" +
			"- artifact：仅上传安装包，不创建任何版本（用于验证凭据与文件是否被接受）\n" +
			"- draft：停在草稿态，可先到渠道后台核对再送审\n" +
			"- submit：一路走到送审（默认）\n" +
			"不指定则各渠道走到各自能到的最远阶段。" +
			"注意并非所有渠道都支持中途停下：小米的 dev/push 是原子请求，只能 submit；" +
			"OPPO 与 vivo 没有草稿态，只支持 artifact 或 submit。" +
			"调用前请先看 list_channels 返回的 capability.supportedStages。",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: &destructive,
			IdempotentHint:  false,
			OpenWorldHint:   &openWorld,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, uploadOutput, error) {
		applicationID, err := artifact.ValidateApplicationID(in.ApplicationID)
		if err != nil {
			return errorResult(err, func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
		}

		var stopPtr *channel.ReleaseStage
		if strings.TrimSpace(in.StopAfter) != "" {
			stage, specified, err := channel.ParseStage(in.StopAfter)
			if err != nil {
				return errorResult(err, func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
			}
			if specified {
				stopPtr = &stage
			}
		}

		// 先解析本次会走到哪一步，再决定要不要 confirm。
		// 停在送审之前不产生不可撤销的副作用，不该被同一个门槛拦住
		stages, err := svc.ResolvedStages(applicationID, in.Channels, stopPtr)
		if err != nil {
			return errorResult(err, func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
		}
		willSubmit := false
		targets := make([]string, 0, len(stages))
		for _, ch := range channel.All() {
			stage, ok := stages[ch.ID()]
			if !ok {
				continue
			}
			targets = append(targets, ch.ID())
			if stage == channel.StageSubmitReview {
				willSubmit = true
			}
		}

		if willSubmit && !in.Confirm {
			return errorResult(eperr.ConfigurationError(
				"送审需要显式传 confirm=true。此操作会向应用商店提交正式版本，"+
					"且各商店均不提供撤销 API。建议先用 check_release 预检；"+
					"若只想验证流程是否走得通，可传 stopAfter=artifact 或 draft"),
				func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
		}

		scheduledAt := int64(0)
		if strings.TrimSpace(in.OnlineTime) != "" {
			scheduledAt, err = publish.ParseOnlineTime(in.OnlineTime)
			if err != nil {
				return errorResult(err, func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
			}
		}

		rule := publish.VersionStrict
		if in.AllowSameVersion {
			rule = publish.VersionAllowSame
		}

		jobID, err := svc.Submit(ctx, publish.SubmitOptions{
			ApplicationID: applicationID,
			ArtifactPath:  in.ArtifactPath,
			Release: channel.ReleaseParams{
				UpdateDesc: in.UpdateDesc,
				OnlineTime: scheduledAt,
			},
			ChannelIDs: in.Channels,
			Version:    rule,
			Timeouts:   timeoutsOf(in.TimeoutSec),
			StopAfter:  stopPtr,
		})
		if err != nil {
			return errorResult(err, func(e *errorPayload) uploadOutput { return uploadOutput{Error: e} })
		}

		out := uploadOutput{
			JobID:        jobID,
			TargetStages: map[string]string{},
			Channels:     targets,
		}
		for id, stage := range stages {
			out.TargetStages[id] = stage.String()
		}
		if stopPtr != nil {
			out.RequestedStage = stopPtr.String()
		}
		if willSubmit {
			out.Note = "用 get_upload_status 轮询进度。上传大包可能需要数分钟到数十分钟。"
		} else {
			out.Note = "本次不含送审。用 get_upload_status 轮询；" +
				"完成后各渠道的 reachedStage 会显示实际到达的阶段。"
		}
		return nil, out, nil
	})
}

// ---- get_upload_status ----

type uploadStatusInput struct {
	JobID string `json:"jobId" jsonschema:"upload_apk 返回的任务 id"`
}

type uploadStatusChannel struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"displayName"`
	Stage             string   `json:"stage"`
	Message           string   `json:"message"`
	ReachedStage      string   `json:"reachedStage,omitempty"`
	ReachedStageLabel string   `json:"reachedStageLabel,omitempty"`
	Kind              string   `json:"kind,omitempty"`
	Code              string   `json:"code,omitempty"`
	Retryable         *bool    `json:"retryable,omitempty"`
	Phase             string   `json:"phase,omitempty"`
	Detail            string   `json:"detail,omitempty"`
	Progress          *float64 `json:"progress,omitempty"`
}

type uploadStatusOutput struct {
	// Error 非空表示本次调用失败。结构化错误必须放在这里 ——
	// SDK 会用输出结构体覆盖 structuredContent
	Error          *errorPayload         `json:"error,omitempty"`
	JobID          string                `json:"jobId"`
	State          string                `json:"state"`
	Done           bool                  `json:"done"`
	ApplicationID  string                `json:"applicationId"`
	VersionCode    int64                 `json:"versionCode"`
	VersionName    string                `json:"versionName"`
	RequestedStage string                `json:"requestedStage,omitempty"`
	Channels       []uploadStatusChannel `json:"channels"`
}

func registerGetUploadStatus(server *mcp.Server, svc *publish.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_upload_status",
		Description: "查询 upload_apk 返回的任务进度。" +
			"state 为 Running 时应稍后再查（建议间隔 5 秒以上）；" +
			"Succeeded / PartiallyFailed / Failed / Cancelled 为终态。",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in uploadStatusInput) (*mcp.CallToolResult, uploadStatusOutput, error) {
		job, ok := svc.Job(in.JobID)
		if !ok {
			return errorResult(eperr.ConfigurationError(
				"任务不存在：%s", in.JobID), func(e *errorPayload) uploadStatusOutput { return uploadStatusOutput{Error: e} })
		}

		out := uploadStatusOutput{
			JobID: job.ID, State: job.State().String(), Done: job.Done(),
			ApplicationID: job.ApplicationID,
			VersionCode:   job.VersionCode, VersionName: job.VersionName,
			Channels: []uploadStatusChannel{},
		}
		if job.RequestedStage != nil {
			out.RequestedStage = job.RequestedStage.String()
		}

		for _, c := range job.Channels {
			item := uploadStatusChannel{
				ID: c.ChannelID, DisplayName: c.DisplayName,
				Stage: c.Stage.Kind.String(), Message: c.Stage.Label(),
			}
			switch c.Stage.Kind {
			case publish.StageSucceeded:
				item.ReachedStage = c.Stage.Reached.String()
				item.ReachedStageLabel = c.Stage.Reached.Label()
			case publish.StageUploading:
				progress := c.Stage.Fraction
				item.Progress = &progress
			case publish.StageFailed:
				item.Kind = c.Stage.ErrKind.String()
				item.Code = c.Stage.Code
				retry := c.Stage.Retryable
				item.Retryable = &retry
				// phase 解释「为什么不可重试」
				item.Phase = c.Stage.Phase.String()
				item.Detail = c.Stage.Message
			}
			out.Channels = append(out.Channels, item)
		}
		return nil, out, nil
	})
}

// ---- 辅助 ----

// errorPayload 是失败结果的结构化形状。
//
// 为什么不直接返回 Go error：SDK 会把它转成纯文本的 content，丢掉 kind / retryable /
// phase —— 而这三项正是调用方（通常是模型）决定「重试、换渠道还是交给人」的依据。
// 「网络超时」和「已越过送审点的超时」在文本上看起来差不多，但一个可以重试、
// 另一个绝不能。
type errorPayload struct {
	OK        bool   `json:"ok"`
	Kind      string `json:"kind"`
	Channel   string `json:"channel,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	// Phase 解释「为什么不可重试」：越过送审点后无法确定服务端是否已受理，
	// 重试可能造成重复版本
	Phase string `json:"phase"`
	Raw   string `json:"rawResponse,omitempty"`
}

// errorResult 把错误转成结构化结果。
//
// ## 为什么错误要放进输出结构体
//
// SDK 会把输出结构体序列化进 structuredContent，**并覆盖**调用方在
// CallToolResult 上设置的 StructuredContent。所以结构化错误必须作为输出的一部分，
// 而不是挂在结果对象上 —— 否则调用方只能拿到一段文本。
//
// ## 为什么需要结构化
//
// 「网络超时」与「已越过送审点的超时」在文本上看起来差不多，
// 但一个可以重试、另一个绝不能。kind / retryable / phase 是调用方
// 决定「重试、换渠道还是交给人」的依据。
//
// 返回的 error 恒为 nil —— 错误已经通过结果传达，不该再走 SDK 的文本转换路径。
func errorResult[T any](err error, build func(*errorPayload) T) (*mcp.CallToolResult, T, error) {
	payload := errorPayloadOf(err)
	text, marshalErr := json.MarshalIndent(payload, "", "  ")
	if marshalErr != nil {
		text = []byte(payload.Message)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
		IsError: true,
	}, build(&payload), nil
}

// errorPayloadOf 把任意 error 归一化成结构化形状。
func errorPayloadOf(err error) errorPayload {
	payload := errorPayload{OK: false}

	var pe *eperr.Error
	if errors.As(err, &pe) {
		payload.Kind = pe.Kind.String()
		payload.Channel = pe.Channel
		payload.Code = pe.Code
		payload.Message = pe.Msg
		payload.Retryable = pe.Retryable()
		payload.Phase = pe.Phase.String()
		if len(pe.Raw) > 1000 {
			payload.Raw = pe.Raw[:1000]
		} else {
			payload.Raw = pe.Raw
		}
	} else {
		payload.Kind = eperr.KindUnknown.String()
		payload.Message = err.Error()
		payload.Retryable = true
		payload.Phase = eperr.PhasePreSubmission.String()
	}

	return payload
}

func readOnly() *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: &openWorld,
	}
}

func timeoutsOf(seconds int64) httpx.Timeouts {
	if seconds > 0 {
		return httpx.OfSeconds(seconds)
	}
	return httpx.Default()
}
