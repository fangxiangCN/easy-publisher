package mi

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
	// ParamAccount 是小米开放平台账号（邮箱）
	ParamAccount = "account"
	// ParamPublicKey 是开放平台下载的 .cer 公钥证书
	ParamPublicKey = "publicKey"
	// ParamPrivateKey 是开放平台的「API 密码」，接口字段名是 password
	ParamPrivateKey = "privateKey"
)

// Channel 是小米应用市场渠道。无状态，可被注册表共享。
type Channel struct {
	baseURL   string
	newClient func(httpx.Timeouts) *http.Client
}

// New 构造小米渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的小米渠道。
func NewWithBaseURL(baseURL string) *Channel { return &Channel{baseURL: baseURL} }

func (c *Channel) ID() string          { return ID }
func (c *Channel) DisplayName() string { return "小米" }
func (c *Channel) FileNameTag() string { return "MI" }

func (c *Channel) client(t httpx.Timeouts) *http.Client {
	if c.newClient != nil {
		return c.newClient(t)
	}
	return httpx.Client(t)
}

func (c *Channel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamAccount,
			Description: "小米开放平台账号（邮箱）",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name:        ParamPublicKey,
			Description: "公钥证书，小米开放平台下载的 .cer 文件",
			Required:    true,
			Type:        channel.ParamTextFile,
			// 文件型参数：内容是证书文本，从文件读入
			FileExtension: "cer",
		},
		{
			Name:        ParamPrivateKey,
			Description: "私钥（开放平台「API 密码」）",
			Required:    true,
			Type:        channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		// dev/push 是原子的，没有任何可停下的中间位置
		SupportedStages: []channel.ReleaseStage{channel.StageSubmitReview},
		RiskLevel:       channel.RiskCritical,
		// 小米既没有草稿态，也没有公开的撤回接口
		Withdrawal:                    channel.WithdrawalNotVerified,
		RequiresExplicitConfirmation:  true,
		AutomaticRetryAfterSubmission: false,
		// 小米是六个渠道里唯一完全未用真实凭据验证过的
		Evidence: channel.EvidenceCodeObservation,
		Note: "dev/push 把「上传安装包」与「提交审核」合并为一次请求，中间无处可停 —— " +
			"一旦调用即送审，无法先建草稿确认。",
	}
}

// Upload 上传 APK 并提交审核。
//
// 小米的 dev/push 是原子操作：上传与送审合并在一次请求里，因此整个调用都属于
// 不可撤销区间。
//
// 这里有个已知的取舍：若失败发生在上传途中，服务端几乎肯定没有受理，此时重试
// 其实是安全的。但客户端无法区分「上传中断」与「已受理但响应丢失」，
// 而两者代价不对称 —— 重复送审不可撤销，多做一次人工确认只是麻烦。
// 因此一律按不可重试处理，并在提示里说明需要先到后台确认。
func (c *Channel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	if err := channel.RequireSupportedStage(c, req.StopAfter); err != nil {
		return 0, err
	}
	account, err := req.Credentials.Get(ParamAccount)
	if err != nil {
		return 0, err
	}
	certificate, err := req.Credentials.Get(ParamPublicKey)
	if err != nil {
		return 0, err
	}
	password, err := req.Credentials.Get(ParamPrivateKey)
	if err != nil {
		return 0, err
	}

	api := NewAPI(c.client(req.Timeouts), c.baseURL)
	packageName := req.ArtifactInfo.ApplicationID

	// 日志里只出现账号与包名；证书、私钥、SIG 一律不落盘
	logx.Info("开始提交新版本",
		"applicationId", packageName,
		"account", logx.Redact(account))

	// 上传需要 appName / packageName，小米只在查询接口返回，所以先查一次
	appInfo, err := api.GetAppInfo(ctx, account, certificate, password, packageName)
	if err != nil {
		return 0, err
	}
	info, err := appInfo.requirePackageInfo()
	if err != nil {
		return 0, err
	}

	err = eperr.AtSubmissionPoint(ctx, ID, "小米", "上传并提交审核", func(ctx context.Context) error {
		return api.UploadApk(ctx, account, certificate, password,
			req.ArtifactFile, info,
			req.ReleaseParams.UpdateDesc, req.ReleaseParams.OnlineTime,
			req.OnProgress)
	})
	if err != nil {
		return 0, err
	}

	logx.Info("提交成功", "applicationId", packageName)
	return channel.StageSubmitReview, nil
}

func (c *Channel) QueryMarket(ctx context.Context, q channel.MarketQuery) (channel.MarketInfo, error) {
	account, err := q.Credentials.Get(ParamAccount)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	certificate, err := q.Credentials.Get(ParamPublicKey)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	password, err := q.Credentials.Get(ParamPrivateKey)
	if err != nil {
		return channel.MarketInfo{}, err
	}

	api := NewAPI(c.client(q.Timeouts), c.baseURL)
	resp, err := api.GetAppInfo(ctx, account, certificate, password, q.ApplicationID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	return resp.ToMarketInfo(), nil
}

// ToMarketInfo 映射为统一的市场状态。
//
// updateVersion 缺失时不能猜 —— 猜 true 会让上层误以为可以提交，猜 false 又会
// 拦住正常发布，所以归为 Unknown。版本号缺失时 LastVersion 传 nil，不伪造 0。
func (r AppInfoResp) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw := "updateVersion=null"
	if r.UpdateVersion != nil {
		raw = "updateVersion=" + boolText(*r.UpdateVersion)
		if *r.UpdateVersion {
			state = channel.ReviewOnline
		} else {
			state = channel.ReviewUnderReview
		}
	}

	var version *channel.Version
	if r.PackageInfo != nil && r.PackageInfo.VersionCode != nil {
		version = &channel.Version{
			Code: *r.PackageInfo.VersionCode,
			Name: r.PackageInfo.VersionName,
		}
	}
	return channel.NewMarketInfo(ID, state, version, raw)
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

var _ channel.Channel = (*Channel)(nil)
