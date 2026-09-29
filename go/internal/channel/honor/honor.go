package honor

import (
	"context"
	"net/http"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

const (
	// ParamClientID 是荣耀开放平台的 client_id 参数名
	ParamClientID = "client_id"
	// ParamClientSecret 是荣耀开放平台的 client_secret 参数名
	ParamClientSecret = "client_secret"
)

// Channel 是荣耀应用市场渠道。无状态，可被注册表共享。
type Channel struct {
	// baseURL / tokenURL 与 client 可注入，用于测试与联调。
	//
	// Kotlin 版把这两个域名写成常量，渠道流程完全无法集成测试 ——
	// 这处改动让 httptest 能断言完整的请求形状与调用顺序。
	baseURL   string
	tokenURL  string
	newClient func(httpx.Timeouts) *http.Client
}

// New 构造荣耀渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的荣耀渠道。tokenURL 为空时沿用默认。
func NewWithBaseURL(baseURL, tokenURL string) *Channel {
	return &Channel{baseURL: baseURL, tokenURL: tokenURL}
}

func (c *Channel) ID() string          { return ID }
func (c *Channel) DisplayName() string { return "荣耀" }
func (c *Channel) FileNameTag() string { return "HONOR" }

func (c *Channel) client(t httpx.Timeouts) *http.Client {
	if c.newClient != nil {
		return c.newClient(t)
	}
	return httpx.Client(t)
}

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamClientID,
			Description: "荣耀开放平台的 client_id",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name:        ParamClientSecret,
			Description: "荣耀开放平台的 client_secret",
			Required:    true,
			Type:        channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		// 上传与送审分离：绑定文件并更新版本描述后即形成草稿
		SupportedStages: []channel.ReleaseStage{
			channel.StageUploadArtifact,
			channel.StageCreateDraft,
			channel.StageSubmitReview,
		},
		RiskLevel:                     channel.RiskHigh,
		Withdrawal:                    channel.WithdrawalNotVerified,
		RequiresExplicitConfirmation:  true,
		AutomaticRetryAfterSubmission: false,
		Evidence:                      channel.EvidenceVerifiedInProduction,
		VerifiedScope:                 "鉴权、APK 上传、绑定草稿已用真实凭据跑通；送审未验证",
		Note: "上传与送审分离：绑定文件并更新版本描述后即形成草稿，可在荣耀开发者后台" +
			"确认后再送审。releaseType=1 审核通过后立即发布，2 为定时发布。",
	}
}

func (c *Channel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

// Upload 执行发布流程。
//
// 步骤顺序与原实现一致：取 token → 取 appId → 取语言信息 → 申请上传地址 →
// 上传 → 绑定文件 → 改更新说明 → 提交审核。荣耀要求「先绑定文件再改语言信息」，
// 顺序变动会导致提交的版本缺内容，故保持原样。
func (c *Channel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	if err := channel.RequireSupportedStage(c, req.StopAfter); err != nil {
		return 0, err
	}
	clientID, err := req.Credentials.Get(ParamClientID)
	if err != nil {
		return 0, err
	}
	clientSecret, err := req.Credentials.Get(ParamClientSecret)
	if err != nil {
		return 0, err
	}
	api := NewAPI(c.client(req.Timeouts), c.baseURL, c.tokenURL)

	logx.Info("开始提交新版本",
		"applicationId", req.ArtifactInfo.ApplicationID,
		"version", req.ArtifactInfo.VersionName)

	token, err := c.bearerToken(ctx, api, clientID, clientSecret)
	if err != nil {
		return 0, err
	}
	appID, err := api.GetAppID(ctx, token, req.ArtifactInfo.ApplicationID)
	if err != nil {
		return 0, err
	}
	lang, err := firstLanguage(ctx, api, token, appID)
	if err != nil {
		return 0, err
	}
	target, err := api.GetUploadURL(ctx, token, appID, req.ArtifactFile)
	if err != nil {
		return 0, err
	}
	if err := api.UploadFile(ctx, token, target, req.ArtifactFile, req.OnProgress); err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageUploadArtifact {
		// 文件已在荣耀的对象存储里，但尚未绑定到版本，不产生任何可检视的版本
		logx.Info("已按请求停在「仅上传安装包」：未绑定文件、未创建版本")
		return channel.StageUploadArtifact, nil
	}

	if err := api.BindApkFile(ctx, token, appID, target); err != nil {
		return 0, err
	}
	if err := api.UpdateVersionDesc(ctx, token, appID, req.ReleaseParams.UpdateDesc, lang); err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageCreateDraft {
		// 此时草稿已完整：文件已绑定、更新描述已写入。
		// 停在这里的价值是让人先到开发者后台核对，再决定是否送审 ——
		// 送审不可撤销，而草稿可以随时改
		logx.Info("草稿已就绪（文件已绑定、更新描述已写入），未送审。" +
			"可到荣耀开发者后台核对后再执行送审")
		return channel.StageCreateDraft, nil
	}

	// 年龄分级是送审的必填前置。未设置时 submit-audit 报 `app rating id is empty`，
	// 而该错误的提示里既不说去哪设、也不说是哪个字段 —— 实测只能靠比对
	// get-app-detail 的 basicInfo.ratingId 是否为 null 才能定位。
	// 因此在这里主动补齐：已设置则不动，避免覆盖运营在后台选定的等级。
	fixed, err := api.EnsureRating(ctx, token, appID)
	if err != nil {
		return 0, err
	}
	if fixed {
		logx.Info("荣耀应用未设置年龄分级，已补为默认值（3+）。"+
			"如需其他等级请在荣耀开发者后台修改", "ratingId", DefaultRatingId)
	}

	// 送审不可撤销，之后的失败一律标记为不可重试
	err = eperr.AtSubmissionPoint(ctx, ID, "荣耀", "提交审核", func(ctx context.Context) error {
		return api.Submit(ctx, token, appID, req.ReleaseParams.OnlineTime)
	})
	if err != nil {
		return 0, err
	}

	logx.Info("新版本已提交审核", "applicationId", req.ArtifactInfo.ApplicationID)
	return channel.StageSubmitReview, nil
}

func (c *Channel) QueryMarket(ctx context.Context, q channel.MarketQuery) (channel.MarketInfo, error) {
	clientID, err := q.Credentials.Get(ParamClientID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	clientSecret, err := q.Credentials.Get(ParamClientSecret)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	api := NewAPI(c.client(q.Timeouts), c.baseURL, c.tokenURL)

	token, err := c.bearerToken(ctx, api, clientID, clientSecret)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	appID, err := api.GetAppID(ctx, token, q.ApplicationID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	state, err := api.GetCurrentRelease(ctx, token, appID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	return state.ToMarketInfo(), nil
}

// bearerToken 取 token 并拼成 Authorization 头的值。
//
// 只记录脱敏后的片段：日志文件长期留存，明文 access_token 等同于凭据泄露。
func (c *Channel) bearerToken(
	ctx context.Context,
	api *API,
	clientID, clientSecret string,
) (string, error) {
	raw, err := api.GetToken(ctx, clientID, clientSecret)
	if err != nil {
		return "", err
	}
	logx.Debug("获取token成功", "token", logx.Redact(raw))
	return "Bearer " + raw, nil
}

// firstLanguage 取第一条语言信息，用于改更新说明时回填 appName / intro。
//
// 原实现是 .languageInfo.first()：字段缺失抛 JsonDataException，空数组抛
// NoSuchElementException，两种情况用户都只看到一句英文异常。
// 这里给带可执行动作的中文提示。
func firstLanguage(
	ctx context.Context,
	api *API,
	token, appID string,
) (LanguageInfo, error) {
	info, err := api.GetAppInfo(ctx, token, appID)
	if err != nil {
		return LanguageInfo{}, err
	}
	if len(info.LanguageInfo) == 0 {
		return LanguageInfo{}, eperr.PreconditionError(ID,
			"荣耀未返回应用的语言信息（appId=%s），请先在荣耀开发者后台补全应用介绍后再发版", appID)
	}
	return info.LanguageInfo[0], nil
}

var _ channel.Channel = (*Channel)(nil)
