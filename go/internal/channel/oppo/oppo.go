package oppo

import (
	"context"
	"net/http"
	"strconv"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

const (
	// ParamClientID 是 OPPO 开放平台的 client_id 参数名
	ParamClientID = "client_id"
	// ParamClientSecret 是 OPPO 开放平台的 client_secret 参数名
	ParamClientSecret = "client_secret"
)

// Channel 是 OPPO 应用市场渠道。无状态，可被注册表共享。
type Channel struct {
	// baseURL 可注入：测试指向 httptest.Server，联调可指向其他环境
	baseURL string
	// newClient 仅供测试注入。
	//
	// 测试服务器若用 httptest.NewTLSServer，它的证书是自签的，需要 srv.Client()
	// 才能信任；而 OPPO 的 requireHTTPS 又拒绝明文地址，所以测试必须用 TLS 服务器
	// 且必须能替换客户端。生产路径永远走 httpx.Client。
	newClient func(httpx.Timeouts) *http.Client
}

// New 构造 OPPO 渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的 OPPO 渠道。
func NewWithBaseURL(baseURL string) *Channel { return &Channel{baseURL: baseURL} }

func (c *Channel) client(t httpx.Timeouts) *http.Client {
	if c.newClient != nil {
		return c.newClient(t)
	}
	return httpx.Client(t)
}

func (c *Channel) ID() string          { return ID }
func (c *Channel) DisplayName() string { return "OPPO" }
func (c *Channel) FileNameTag() string { return "OPPO" }

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamClientID,
			Description: "OPPO 开放平台的 client_id，在「账号管理 - API 密钥」中获取",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name: ParamClientSecret,
			Description: "OPPO 开放平台的 client_secret。注意该凭据会出现在取 token 的 URL query 上" +
				"（OPPO 接口设计如此，未能查证是否支持 POST form），建议缩短轮换周期",
			Required: true,
			Type:     channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		// 文件上传与 app/upd 是分开的，但后者一步完成版本更新与送审，
		// 中间没有可检视的草稿态
		SupportedStages: []channel.ReleaseStage{
			channel.StageUploadArtifact,
			channel.StageSubmitReview,
		},
		RiskLevel:                     channel.RiskCritical,
		Withdrawal:                    channel.WithdrawalNotVerified,
		RequiresExplicitConfirmation:  true,
		AutomaticRetryAfterSubmission: false,
		Evidence:                      channel.EvidenceVerifiedInProduction,
		VerifiedScope:                 "鉴权、签名请求、文件上传已用真实凭据跑通；送审（app/upd）未验证",
		Note: "app/upd 是全量更新语义：必须把从 app/info 读回的图标、截图、介绍、分类、" +
			"软著等字段原样回传，漏任何一个会被清空或被拒。没有草稿态可检视；" +
			"单独上传安装包只能验证凭据与文件是否被接受。",
	}
}

func (c *Channel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

// validateStoreFields 校验提交版本必须回传的商店资料。
//
// 与「结构性缺失」（access_token、upload_url、sign）区分开：那些意味着接口变更，
// 而这些意味着商店里的应用资料没填全 —— 给可执行的中文提示。
func validateStoreFields(info AppInfo) error {
	_, err := requireAppFields(info)
	return err
}

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
	api := NewAPI(clientID, clientSecret, c.client(req.Timeouts), c.baseURL)

	// 只打印脱敏后的 client_id：client_secret 与 access_token 都不得进日志
	logx.Debug("获取 token", "client_id", logx.Redact(clientID))

	token, err := api.GetToken(ctx)
	if err != nil {
		return 0, err
	}

	// 提交版本要求全量字段，必须先把商店里现有的资料读回来
	appInfo, err := api.GetAppInfo(ctx, token, req.ArtifactInfo.ApplicationID)
	if err != nil {
		return 0, err
	}

	// 商店资料是否齐全，放在上传之前判定。
	// 否则会先传完一个上百兆的包，才发现后台缺图标 —— 白等一轮，
	// 而且中间中断留下的是一个已上传但未提交的孤儿文件
	if err := validateStoreFields(appInfo); err != nil {
		return 0, err
	}

	target, err := api.GetUploadURL(ctx, token)
	if err != nil {
		return 0, err
	}

	result, err := api.UploadAPK(ctx, target, token, req.ArtifactFile, req.OnProgress)
	if err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageUploadArtifact {
		// OPPO 没有草稿态，停在这里只能证明凭据、签名与文件被接受，
		// 不会在后台留下任何可查看的版本
		logx.Info("已按请求停在「仅上传安装包」：文件已被 OPPO 接受，未提交版本")
		return channel.StageUploadArtifact, nil
	}

	// app/upd 是送审动作，不可撤销。响应解析失败也属于「不知道有没有受理」，
	// 同样不能标记为可重试
	err = eperr.AtSubmissionPoint(ctx, ID, "OPPO", "提交版本", func(ctx context.Context) error {
		return api.Submit(ctx, token, req.ArtifactInfo, appInfo, ReleaseParams{
			UpdateDesc: req.ReleaseParams.UpdateDesc,
			OnlineTime: req.ReleaseParams.OnlineTime,
		}, result)
	})
	if err != nil {
		return 0, err
	}

	// app/upd 是异步接口：errno=0 只代表任务入队。必须轮询才能真正知道版本
	// 有没有创建成功 —— 否则缺必传参数、截图尺寸超标之类的失败会被当成成功上报，
	// 线上版本号纹丝不动却没有任何报错。
	//
	// 轮询失败意味着任务确定失败（渠道给出了原因），此时版本没有改变，
	// 因此不算「越过送审点」，不套 AtSubmissionPoint。
	if err := api.WaitSubmitResult(ctx, token,
		req.ArtifactInfo.ApplicationID, strconv.FormatInt(req.ArtifactInfo.VersionCode, 10)); err != nil {
		return 0, err
	}

	// 绝不回显参数值
	logx.Info("已提交新版本",
		"applicationId", req.ArtifactInfo.ApplicationID,
		"version", req.ArtifactInfo.VersionName,
		"versionCode", req.ArtifactInfo.VersionCode)
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
	api := NewAPI(clientID, clientSecret, c.client(q.Timeouts), c.baseURL)

	token, err := api.GetToken(ctx)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	info, err := api.GetAppInfo(ctx, token, q.ApplicationID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	return info.ToMarketInfo(), nil
}

var _ channel.Channel = (*Channel)(nil)
