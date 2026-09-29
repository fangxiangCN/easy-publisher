package harmony

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

const (
	// ParamClientID / ParamClientSecret 与华为渠道同一套凭据
	ParamClientID     = "client_id"
	ParamClientSecret = "client_secret"
	// ParamAppID 是鸿蒙应用在 AGC 里的 appId
	ParamAppID = "app_id"
	// ParamRegisteredIDType / ParamRegisteredIDNumber 是可选的主体登记信息
	ParamRegisteredIDType   = "registered_id_type"
	ParamRegisteredIDNumber = "registered_id_number"

	// compileRetryInterval 是遇到「软件包尚未编译完成」时的重试间隔
	compileRetryInterval = 20 * time.Second
	// compileMaxRetries 是上述重试的次数上限
	compileMaxRetries = 6

	// progressLogEvery 每上传多少片记一条日志
	progressLogEvery = 10
)

// Channel 是鸿蒙 AppGallery 渠道。无状态，可被注册表共享。
type Channel struct {
	baseURL   string
	newClient func(httpx.Timeouts) *http.Client

	// retryInterval / maxRetries 可注入，让重试路径的测试不必真等 20 秒
	retryInterval *time.Duration
	maxRetries    *int
}

// New 构造鸿蒙渠道，使用生产域名。
func New() *Channel { return &Channel{} }

// NewWithBaseURL 构造指向指定网关的鸿蒙渠道。
func NewWithBaseURL(baseURL string) *Channel { return &Channel{baseURL: baseURL} }

func (c *Channel) ID() string { return ID }

func (c *Channel) DisplayName() string { return "鸿蒙" }

func (c *Channel) FileNameTag() string { return "HARMONY" }

func (c *Channel) client(t httpx.Timeouts) *http.Client {
	if c.newClient != nil {
		return c.newClient(t)
	}
	return httpx.Client(t)
}

func (c *Channel) retryPolicy() (time.Duration, int) {
	interval, retries := compileRetryInterval, compileMaxRetries
	if c.retryInterval != nil {
		interval = *c.retryInterval
	}
	if c.maxRetries != nil {
		retries = *c.maxRetries
	}
	return interval, retries
}

// ArtifactKinds 只接受 .app（App Pack）。
//
// HAP 是旧的单模块包，不是 HarmonyOS 5+ 的商店发布包，因此不接受。
func (c *Channel) ArtifactKinds() []artifact.Kind {
	return []artifact.Kind{artifact.KindHarmonyAppPack}
}

func (c *Channel) Params() []channel.ChannelParam {
	return []channel.ChannelParam{
		{
			Name:        ParamClientID,
			Description: "AGC 项目的客户端 ID（与华为渠道同一套凭据）",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name:        ParamClientSecret,
			Description: "AGC 项目的客户端密钥",
			Required:    true,
			Type:        channel.ParamText,
		},
		{
			Name: ParamAppID,
			Description: "鸿蒙应用在 AGC 里的 appId。注意与同名 Android 应用不是同一个 id，" +
				"需到 AGC 后台「我的应用」里查看鸿蒙应用条目",
			Required: true,
			Type:     channel.ParamText,
		},
		{
			Name: ParamRegisteredIDType,
			Description: "主体登记信息类型（可选）。送审报 " +
				"「registeredIdType and registeredIdNumber can not be null」时才需要填",
			Required: false,
			Type:     channel.ParamText,
		},
		{
			Name:        ParamRegisteredIDNumber,
			Description: "主体登记号码（可选），与 registered_id_type 成对填写",
			Required:    false,
			Type:        channel.ParamText,
		},
	}
}

func (c *Channel) Capabilities() channel.Capabilities {
	return channel.Capabilities{
		SupportedStages: []channel.ReleaseStage{
			channel.StageUploadArtifact,
			channel.StageCreateDraft,
			channel.StageSubmitReview,
		},
		RiskLevel: channel.RiskHigh,
		// 华为 AGC 文档里有「撤销审核」接口，平台层面支持撤回；
		// 但本工具尚未实现该调用，且其适用条件未核实
		Withdrawal:                    channel.WithdrawalAPISupported,
		RequiresExplicitConfirmation:  true,
		AutomaticRetryAfterSubmission: false,
		Evidence:                      channel.EvidenceVerifiedInProduction,
		VerifiedScope: "鉴权、App Pack 分片上传、v3 关联草稿已用真实凭据跑通；" +
			"送审（v3 app-submit）未验证",
		Note: "走 AGC 的 v3 接口（Android 版是 v2，两者不可混用）。" +
			"appId 必须显式配置：鸿蒙应用在 AGC 里是独立记录，用包名反查会拿到 " +
			"Android 应用的 id。商店里的「新版本介绍」需人工维护 —— v3 语言信息接口" +
			"未经验证，暂未实现；updateDesc 仅在长度符合 10-300 字时作为提审备注提交。",
	}
}

// Upload 执行发布流程。
//
// token → init → 逐片算摘要 → parts 换地址 → 逐片上传 → compose → 关联草稿 → 送审
func (c *Channel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	if err := channel.RequireSupportedStage(c, req.StopAfter); err != nil {
		return 0, err
	}
	if req.ArtifactInfo.Kind != artifact.KindHarmonyAppPack {
		return 0, eperr.LocalFileError(
			"鸿蒙渠道需要 .app 格式的 App Pack，收到的是 %s（%s）",
			req.ArtifactInfo.Kind, req.ArtifactInfo.FileName())
	}

	clientID, err := req.Credentials.Get(ParamClientID)
	if err != nil {
		return 0, err
	}
	clientSecret, err := req.Credentials.Get(ParamClientSecret)
	if err != nil {
		return 0, err
	}
	appID, err := req.Credentials.Get(ParamAppID)
	if err != nil {
		return 0, err
	}

	// 鸿蒙拿不到市场状态，版本号比对做不了。明确记一条日志，
	// 不要让人以为已经校验过了
	logx.Info("鸿蒙渠道不支持查询市场状态，本次跳过线上版本号比对",
		"version", req.ArtifactInfo.VersionName,
		"versionCode", req.ArtifactInfo.VersionCode)
	logx.Info("开始上传 App Pack", "appId", appID,
		"account", logx.Redact(clientID))

	api := NewAPI(c.client(req.Timeouts), c.baseURL)
	token, err := api.GetToken(ctx, clientID, clientSecret)
	if err != nil {
		return 0, err
	}
	auth := authHeader{token: "Bearer " + token, clientID: clientID}

	objectID, err := c.uploadAppPack(ctx, api, auth, appID, req)
	if err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageUploadArtifact {
		// 文件已在华为的对象存储里，但没有关联到任何草稿版本，
		// AGC 后台看不到它。用途是验证凭据、appId 与包体是否被接受
		logx.Info("已按请求停在「仅上传」：未关联草稿", "objectId", objectID)
		return channel.StageUploadArtifact, nil
	}

	packageID, err := api.LinkDraft(ctx, auth, appID, req.ArtifactInfo.FileName(), objectID)
	if err != nil {
		return 0, err
	}

	if req.StopAfter == channel.StageCreateDraft {
		logx.Info("草稿已就绪，未送审。请到 AGC 后台核对后再手动提交审核",
			"packageId", packageID)
		return channel.StageCreateDraft, nil
	}

	if err := c.submitForReview(ctx, api, auth, appID, packageID, req); err != nil {
		return 0, err
	}
	logx.Info("已提交审核", "appId", appID)
	return channel.StageSubmitReview, nil
}

// uploadAppPack 完成分片上传的全过程，返回 objectId。
func (c *Channel) uploadAppPack(
	ctx context.Context,
	api *API,
	auth authHeader,
	appID string,
	req channel.UploadRequest,
) (string, error) {
	fileName := req.ArtifactInfo.FileName()

	init, err := api.InitMultipart(ctx, auth, appID, fileName)
	if err != nil {
		return "", err
	}
	// 分片大小必须用华为给的值，自己选会导致 parts 返回的地址与实际分片不匹配
	var partSize int64 = FallbackPartSize
	if init.NspPartMinSize != nil && *init.NspPartMinSize > 0 {
		partSize = *init.NspPartMinSize
	} else {
		logx.Info("华为未返回分片大小，使用兜底值", "bytes", partSize)
	}

	plans, err := planParts(req.ArtifactInfo.SizeBytes, partSize)
	if err != nil {
		return "", err
	}
	logx.Info("开始分片上传", "parts", len(plans), "partSize", partSize)

	f, err := os.Open(req.ArtifactFile)
	if err != nil {
		return "", eperr.LocalFileError("无法打开待上传文件：%s（%v）", req.ArtifactFile, err)
	}
	defer f.Close()

	// 第一遍：算每片摘要。必须在换取上传地址之前完成
	descriptors := make(map[string]PartDescriptor, len(plans))
	for _, plan := range plans {
		data, err := readPartAt(f, plan.Offset, plan.Length)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		descriptors[plan.key()] = PartDescriptor{
			Sha256: hex.EncodeToString(sum[:]),
			Length: plan.Length,
		}
	}

	parts, err := api.GetPartUploadInfo(ctx, auth, init.ObjectID, init.NspUploadID, descriptors)
	if err != nil {
		return "", err
	}

	// 第二遍：逐片上传并收集 ETag
	completed := make(map[string]CompletedPart, len(plans))
	for i, plan := range plans {
		key := plan.key()
		info, ok := parts.UploadInfoMap[key]
		if !ok {
			return "", protocolError(fmt.Sprintf(
				"华为没有为分片 %s 返回上传地址（共 %d 片，返回 %d 条）",
				key, len(plans), len(parts.UploadInfoMap)), "", nil)
		}
		data, err := readPartAt(f, plan.Offset, plan.Length)
		if err != nil {
			return "", err
		}
		etag, err := api.UploadPart(ctx, key, info, data)
		if err != nil {
			return "", err
		}
		completed[key] = CompletedPart{PartObjectID: info.PartObjectID, ETag: etag}

		if req.OnProgress != nil {
			req.OnProgress(float64(i+1) / float64(len(plans)))
		}
		if (i+1)%progressLogEvery == 0 || i+1 == len(plans) {
			logx.Info("分片上传进度", "done", i+1, "total", len(plans))
		}
	}

	if err := api.Compose(ctx, auth, init.ObjectID, init.NspUploadID, completed); err != nil {
		return "", err
	}
	logx.Info("分片已合并", "objectId", init.ObjectID)
	return init.ObjectID, nil
}

// submitForReview 提交发布。
//
// ## 关于 204144660 的自动重试
//
// 该码表示「软件包尚未编译完成就提交」，是华为**明确拒绝**了本次送审 ——
// 结果确定，服务端没有受理，因此等待后重试不会造成重复送审。
//
// 这与网络超时的性质相反：超时是「不知道有没有受理」，那种情况一律不自动重试
// （见 eperr.FailurePhase）。判断能否重试的依据是结果是否确定，
// 而不是错误看起来是否「可恢复」。
func (c *Channel) submitForReview(
	ctx context.Context,
	api *API,
	auth authHeader,
	appID, packageID string,
	req channel.UploadRequest,
) error {
	remark, skippedReason := buildRemark(req.ReleaseParams.UpdateDesc)
	if skippedReason != "" {
		logx.Info(skippedReason, "packageId", packageID)
	}

	payload := SubmitReq{Remark: remark}
	if req.ReleaseParams.OnlineTime > 0 {
		// 与华为 v2 渠道用同一格式：yyyy-MM-dd'T'HH:mm:ssZZ（+0800 无冒号）。
		// 官方 v2 文档与一个可用的 v3 封装实现都这么写；
		// 若定时发布报「时间格式有误」，首先怀疑这里
		formatted := time.UnixMilli(req.ReleaseParams.OnlineTime).
			Format("2006-01-02T15:04:05-0700")
		payload.ReleaseTime = &formatted
	}
	if v := strings.TrimSpace(req.Credentials.Optional(ParamRegisteredIDType)); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			payload.RegisteredIDType = &parsed
		}
	}
	payload.RegisteredIDNumber = req.Credentials.Optional(ParamRegisteredIDNumber)

	interval, maxRetries := c.retryPolicy()
	for attempt := 0; ; attempt++ {
		err := eperr.AtSubmissionPoint(ctx, ID, "鸿蒙", "提交发布", func(ctx context.Context) error {
			return api.Submit(ctx, auth, appID, payload)
		})
		if err == nil {
			return nil
		}

		var pe *eperr.Error
		if asError(err, &pe) && pe.Code == CodePackageNotCompiled && attempt < maxRetries {
			logx.Info("软件包尚未编译完成，稍后重试",
				"code", CodePackageNotCompiled,
				"attempt", attempt+1, "max", maxRetries)
			if err := sleepCtx(ctx, interval); err != nil {
				return err
			}
			continue
		}
		return err
	}
}

// QueryMarket 说明为什么不支持。
func (c *Channel) QueryMarket(ctx context.Context, q channel.MarketQuery) (channel.MarketInfo, error) {
	return channel.MarketInfo{}, unsupportedMarketQuery()
}

// ---- 辅助 ----

func asError(err error, target **eperr.Error) bool {
	return errors.As(err, target)
}

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

var _ channel.Channel = (*Channel)(nil)
