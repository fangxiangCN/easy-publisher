package vivo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

const (
	// ParamAccessKey 是 vivo 开放平台的 access_key 参数名
	ParamAccessKey = "access_key"
	// ParamAccessSecret 是 vivo 开放平台的 access_secret 参数名
	ParamAccessSecret = "access_secret"

	logTag = "vivo应用市场"
)

// getAppInfoWithRetry 查询应用详情，遇 11010 频率限制时自动重试。
//
// 为什么需要在渠道层重试：vivo 的「应用处理中，请勿重复提交」实测是**按包名的
// 查询频率限制** —— 同一包名数秒内重复查询即触发，换包名不受影响，静默几秒
// 自动恢复。而发布流程本身会连续查询（校验一次、上传后再查一次），
// 必然撞上；此前它直接把整个发布任务判为失败。
//
// 重试是安全的：该码不代表有提交在进行，只是限流。若真的代表「有任务在跑」，
// 换包名查询也会被拦，且不会几秒就恢复 —— 实测两者都不成立。
func getAppInfoWithRetry(ctx context.Context, api *API, packageName string) (appInfo, error) {
	const (
		attempts = 5
		interval = 5 * time.Second
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		info, err := api.GetAppInfo(ctx, packageName)
		if err == nil {
			return info, nil
		}
		if !isBusyCode(err) {
			return appInfo{}, err
		}
		lastErr = err
		if i < attempts-1 {
			logx.Debug("vivo 查询被限流，稍后重试", "attempt", i+1, "max", attempts)
			select {
			case <-ctx.Done():
				return appInfo{}, ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return appInfo{}, lastErr
}

// Channel 是 vivo 应用市场渠道。
//
// 无状态：凭据、超时、进度回调全部来自方法入参，实例可被注册表共享，
// 并发上传多个应用不会串台。上游项目的 ChannelTask 是进程级单例却持有可变的
// accessKey / accessSecret / listener，并发场景下会用 A 应用的密钥上传 B 应用的包。
type Channel struct {
	// baseURL 为空时用生产域名。可注入是为了两件事：
	// 测试里指向 httptest.Server（Kotlin 版硬编码 DOMAIN 导致渠道流程无法集成测试），
	// 以及联调时指向沙箱环境。
	baseURL string
}

// New 构造 vivo 渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的 vivo 渠道。
//
// 传 SandboxDomain 可用于联调；测试传 httptest.Server 的 URL。
func NewWithBaseURL(baseURL string) *Channel { return &Channel{baseURL: baseURL} }

func (c *Channel) ID() string          { return ID }
func (c *Channel) DisplayName() string { return "vivo" }
func (c *Channel) FileNameTag() string { return "VIVO" }

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamAccessKey,
			Description: "vivo 开放平台的 access_key，在「开发者服务 - API 调用凭据」中获取",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name:        ParamAccessSecret,
			Description: "vivo 开放平台的 access_secret，与 access_key 配套签发",
			Required:    true,
			Type:        channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		// 文件上传与 app.sync.update.app 是分开的，但后者一步完成版本更新与送审，
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
		VerifiedScope:                 "鉴权、签名请求、文件上传已用真实凭据跑通；送审未验证",
		Note: "app.sync.update.app 一次完成版本更新与送审，没有草稿态。" +
			"onlineType=1 审核通过后立即上架，2 为定时上架。" +
			"签名参数全部走 query（router/rest 网关的强制要求）。",
	}
}

func (c *Channel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

func (c *Channel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	if err := channel.RequireSupportedStage(c, req.StopAfter); err != nil {
		return 0, err
	}
	accessKey, err := req.Credentials.Get(ParamAccessKey)
	if err != nil {
		return 0, err
	}
	accessSecret, err := req.Credentials.Get(ParamAccessSecret)
	if err != nil {
		return 0, err
	}
	api := NewAPI(accessKey, accessSecret, httpx.Client(req.Timeouts), c.baseURL)
	packageName := req.ArtifactInfo.ApplicationID

	// 保持原实现的请求顺序：先查详情再上传。查询会提前暴露包名不属于本账号、
	// 凭据失效这类问题，避免白传一个上百兆的包才失败。
	info, err := step("获取应用信息", func() (appInfo, error) {
		return getAppInfoWithRetry(ctx, api, packageName)
	})
	if err != nil {
		return 0, err
	}
	market := info.ToMarketInfo()
	logx.Info("vivo 线上状态",
		"state", market.ReviewState.Label(),
		"lastVersion", versionOrNone(market.LastVersion))

	result, err := step("上传 APK", func() (UploadResult, error) {
		return api.UploadAPK(ctx, req.ArtifactFile, packageName, req.OnProgress)
	})
	if err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageUploadArtifact {
		// vivo 没有草稿态：停在这里只证明文件与签名被接受
		logx.Info("已按请求停在「仅上传安装包」：文件已被 vivo 接受，未提交版本")
		return channel.StageUploadArtifact, nil
	}

	// 请求一旦发出就进入不可撤销区间。注意连响应解析也纳入保护：
	// 解析失败同样意味着「不知道服务端有没有受理」，不能当成可重试的普通错误。
	err = eperr.AtSubmissionPoint(ctx, ID, "vivo", "提交更新", func(ctx context.Context) error {
		_, inner := step("提交更新", func() (struct{}, error) {
			return struct{}{}, api.Submit(ctx, result, ReleaseParams{
				UpdateDesc: req.ReleaseParams.UpdateDesc,
				OnlineTime: req.ReleaseParams.OnlineTime,
			})
		})
		return inner
	})
	if err != nil {
		return 0, err
	}
	return channel.StageSubmitReview, nil
}

func (c *Channel) QueryMarket(ctx context.Context, q channel.MarketQuery) (channel.MarketInfo, error) {
	accessKey, err := q.Credentials.Get(ParamAccessKey)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	accessSecret, err := q.Credentials.Get(ParamAccessSecret)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	api := NewAPI(accessKey, accessSecret, httpx.Client(q.Timeouts), c.baseURL)

	info, err := getAppInfoWithRetry(ctx, api, q.ApplicationID)
	if err != nil {
		return channel.MarketInfo{}, err
	}
	return info.ToMarketInfo(), nil
}

// step 包裹单个步骤：记日志 + 把底层异常归一成 *eperr.Error。
//
// ctx 取消由调用链上的 http 客户端处理，这里不需要特殊分支 ——
// Go 的 context.Canceled 会原样向上传播，不像 Kotlin 那样必须先 catch 再重抛
// CancellationException，否则用户取消会被当成失败上报。
func step[T any](action string, block func() (T, error)) (T, error) {
	logx.Info(action + " 开始")
	result, err := block()
	if err != nil {
		var pe *eperr.Error
		if !asError(err, &pe) {
			pe = eperr.FromError(ID, err)
		}
		logx.Error(action+" 失败", "err", pe.Describe())
		var zero T
		return zero, pe
	}
	logx.Info(action + " 完成")
	return result, nil
}

func asError(err error, target **eperr.Error) bool {
	return errors.As(err, target)
}

func versionOrNone(v *channel.Version) string {
	if v == nil {
		return "无"
	}
	return fmt.Sprintf("%s(%d)", v.Name, v.Code)
}

var _ channel.Channel = (*Channel)(nil)
