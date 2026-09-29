package huawei

import (
	"context"
	"net/http"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

const (
	// ParamClientID 是华为 AGC 的 client_id 参数名
	ParamClientID = "client_id"
	// ParamClientSecret 是华为 AGC 的 client_secret 参数名
	ParamClientSecret = "client_secret"

	// compileTimeout 是等待 APK 编译完成的上限
	compileTimeout = 3 * time.Minute
	// pollInterval 是轮询间隔
	pollInterval = 10 * time.Second
)

// Channel 是华为 AppGallery 渠道。无状态，可被注册表共享。
type Channel struct {
	baseURL string
	// newClient 仅供测试注入：测试服务器用自签证书，需要 srv.Client()
	newClient func(httpx.Timeouts) *http.Client

	// pollInterval / compileTimeout 可注入，让轮询测试不必真等 3 分钟
	pollTimeouts *pollConfig
}

type pollConfig struct {
	interval time.Duration
	timeout  time.Duration
}

// New 构造华为渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的华为渠道。
func NewWithBaseURL(baseURL string) *Channel { return &Channel{baseURL: baseURL} }

func (c *Channel) ID() string          { return ID }
func (c *Channel) DisplayName() string { return "华为" }
func (c *Channel) FileNameTag() string { return "HUAWEI" }

func (c *Channel) client(t httpx.Timeouts) *http.Client {
	if c.newClient != nil {
		return c.newClient(t)
	}
	return httpx.Client(t)
}

func (c *Channel) poll() pollConfig {
	if c.pollTimeouts != nil {
		return *c.pollTimeouts
	}
	return pollConfig{interval: pollInterval, timeout: compileTimeout}
}

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamClientID,
			Description: "AGC 项目的客户端 ID",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name:        ParamClientSecret,
			Description: "AGC 项目的客户端密钥",
			Required:    true,
			Type:        channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		// 上传与送审分离：绑定 APK 后华为生成草稿版本
		SupportedStages: []channel.ReleaseStage{
			channel.StageUploadArtifact,
			channel.StageCreateDraft,
			channel.StageSubmitReview,
		},
		RiskLevel: channel.RiskHigh,
		// 华为 AGC 文档里有「撤销审核」接口，所以平台层面支持撤回。
		// 但本工具尚未实现该调用，且其适用条件（是否仅限审核中状态）未核实
		Withdrawal:                    channel.WithdrawalAPISupported,
		RequiresExplicitConfirmation:  true,
		AutomaticRetryAfterSubmission: false,
		Evidence:                      channel.EvidenceVerifiedInProduction,
		VerifiedScope:                 "鉴权、APK 上传、绑定草稿已用真实凭据跑通；送审（app-submit）未验证",
		Note: "上传与送审分离：绑定 APK 后华为生成草稿版本，可在 AppGallery Connect " +
			"后台确认后再送审。绑定后有异步编译检查需轮询（最长 3 分钟）。" +
			"AGC 文档提供「撤销审核」接口，但本工具未实现该调用。",
	}
}

func (c *Channel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

// Upload 执行发布流程。
//
// 步骤顺序：取 token → 取 appId → 取上传地址 → 上传 → 绑定 → 等编译 → 改描述 → 送审。
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
	api := NewAPI(c.client(req.Timeouts), c.baseURL)

	token, err := c.bearerToken(ctx, api, clientID, clientSecret)
	if err != nil {
		return 0, err
	}
	appID, err := api.GetAppID(ctx, clientID, token, req.ArtifactInfo.ApplicationID)
	if err != nil {
		return 0, err
	}

	size, err := artifactSize(req.ArtifactFile)
	if err != nil {
		return 0, err
	}
	target, err := api.GetUploadURL(ctx, clientID, token, appID,
		fileNameOf(req.ArtifactFile), size)
	if err != nil {
		return 0, err
	}
	if err := api.UploadFile(ctx, target, req.ArtifactFile, req.OnProgress); err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageUploadArtifact {
		// 文件已在华为的对象存储里，但尚未绑定到应用，不产生任何版本
		logx.Info("已按请求停在「仅上传安装包」：未绑定文件、未创建版本")
		return channel.StageUploadArtifact, nil
	}

	if target.ObjectID == "" {
		return 0, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "华为未返回 objectId，无法绑定已上传的 APK",
		}
	}
	pkgID, err := api.BindApk(ctx, clientID, token, appID, fileNameOf(req.ArtifactFile), target.ObjectID)
	if err != nil {
		return 0, err
	}
	if err := c.waitCompiled(ctx, api, clientID, token, appID, pkgID); err != nil {
		return 0, err
	}
	if err := api.UpdateVersionDesc(ctx, clientID, token, appID, req.ReleaseParams.UpdateDesc); err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageCreateDraft {
		// 此时草稿已完整：文件已绑定、编译检查已通过、更新描述已写入。
		// 停在这里的价值是让人先到 AGC 后台核对，再决定是否送审 ——
		// 送审不可撤销，而草稿可以随时改
		logx.Info("草稿已就绪（文件已绑定并通过编译检查），未送审。" +
			"可到 AppGallery Connect 后台核对后再执行送审")
		return channel.StageCreateDraft, nil
	}

	// 送审不可撤销，之后的失败一律标记为不可重试
	err = eperr.AtSubmissionPoint(ctx, ID, "华为", "提交审核", func(ctx context.Context) error {
		return api.Submit(ctx, clientID, token, appID, req.ReleaseParams.OnlineTime)
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
	api := NewAPI(c.client(q.Timeouts), c.baseURL)

	token, err := c.bearerToken(ctx, api, clientID, clientSecret)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	appID, err := api.GetAppID(ctx, clientID, token, q.ApplicationID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	info, err := api.GetAppInfo(ctx, clientID, token, appID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	market := info.ToMarketInfo()
	logx.Info("应用市场状态",
		"state", market.ReviewState.Label(),
		"lastVersion", versionOrNone(market.LastVersion))
	return market, nil
}

// waitCompiled 轮询等待华为完成 APK 编译。
//
// 相对上游的两处修正：
//   - **先查再等**。上游是 `while(true) { delay(10s); ... }`，等待放在检查之前，
//     且超时判断也在 delay 之后，于是首次检查必然先空等 10 秒。
//   - 空列表视为「尚未拿到状态」并继续等待，而不是抛异常。上游用 first()，
//     华为返回空数组时抛 NoSuchElementException，用户看到毫无信息量的堆栈。
func (c *Channel) waitCompiled(
	ctx context.Context,
	api *API,
	clientID, token, appID, pkgID string,
) error {
	cfg := c.poll()
	deadline := time.Now().Add(cfg.timeout)

	for {
		success, known, err := api.GetCompileState(ctx, clientID, token, appID, pkgID)
		if err != nil {
			return err
		}
		switch {
		case known && success:
			return nil
		case !known:
			logx.Debug("华为暂未返回编译状态（pkgStateList 为空），继续等待")
		}

		if time.Now().After(deadline) {
			return &eperr.Error{
				Kind:    eperr.KindNetwork,
				Channel: ID,
				Msg: "等待华为完成 APK 编译超时（超过 " +
					cfg.timeout.Round(time.Minute).String() + "），" +
					"可稍后在 AppGallery Connect 后台确认包状态",
			}
		}
		if err := sleepCtx(ctx, cfg.interval); err != nil {
			return err
		}
	}
}

// bearerToken 取 token 并拼成 Authorization 头的值。
//
// 只记录脱敏后的片段：上游的 AppLogger.action() 统一 debug("结果:$result")，
// 而这一步的返回值就是裸 access_token，等于把凭据写进日志文件。
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

// sleepCtx 等待指定时长，ctx 取消时立即返回。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func versionOrNone(v *channel.Version) string {
	if v == nil {
		return "无"
	}
	return v.String()
}

var _ channel.Channel = (*Channel)(nil)
