package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

// MarketResult 是单个渠道的状态查询结果。
type MarketResult struct {
	Info channel.MarketInfo
	Err  error
}

// SubmitOptions 是提交参数。
type SubmitOptions struct {
	ApplicationID string
	// ArtifactPath 是制品文件，或存放多渠道包的目录
	ArtifactPath string
	Release      channel.ReleaseParams
	// ChannelIDs 为空表示配置里全部启用的渠道
	ChannelIDs []string
	Version    VersionRule
	Timeouts   httpx.Timeouts
	// StopAfter 为 nil 表示各渠道走到各自能到的最远阶段
	StopAfter *channel.ReleaseStage
}

// Service 编排发布流程。
//
// 对外只接受包名、制品路径与发布参数 —— **绝不接受凭据参数**。凭据由
// config.CredentialStore 在内部读取，避免 secret 出现在 CLI 参数、shell 历史
// 或 agent 的对话上下文里。
type Service struct {
	store    *config.Store
	channels []channel.Channel
	readInfo func(string) (artifact.Info, error)

	// bg 是后台上传任务的根 context
	bg   context.Context
	stop context.CancelFunc
	mu   sync.Mutex
	jobs map[string]*jobHandle
	seq  int
}

// Option 配置 Service。
type Option func(*Service)

// WithChannels 注入渠道列表。默认取注册表；测试可注入替身 ——
// 否则编排逻辑（阶段记录、失败隔离、前置校验时机）完全无法覆盖。
func WithChannels(channels []channel.Channel) Option {
	return func(s *Service) { s.channels = channels }
}

// WithArtifactReader 注入制品解析函数，理由同上。
func WithArtifactReader(fn func(string) (artifact.Info, error)) Option {
	return func(s *Service) { s.readInfo = fn }
}

// NewService 构造编排服务。
func NewService(store *config.Store, opts ...Option) *Service {
	if store == nil {
		store = config.NewStore("")
	}
	bg, stop := context.WithCancel(context.Background())
	s := &Service{
		store:    store,
		channels: channel.All(),
		readInfo: artifact.Read,
		bg:       bg,
		stop:     stop,
		jobs:     map[string]*jobHandle{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Close 取消所有在途任务。
func (s *Service) Close() { s.stop() }

// ListApps 返回全部已配置的应用。
func (s *Service) ListApps() ([]config.AppConfig, error) { return s.store.List() }

// App 读取一个应用配置。
func (s *Service) App(applicationID string) (config.AppConfig, error) {
	return s.store.Require(applicationID)
}

// Channels 返回可用渠道。
func (s *Service) Channels() []channel.Channel { return s.channels }

// ResolvedStages 解析出每个渠道本次会走到哪一步。
//
// 用途是让调用方在动手之前就知道后果 —— 尤其是那些「最远只到草稿」的渠道，
// 以及判断这次操作是否真的包含不可撤销的送审。
func (s *Service) ResolvedStages(
	applicationID string,
	channelIDs []string,
	stopAfter *channel.ReleaseStage,
) (map[string]channel.ReleaseStage, error) {
	cfg, err := s.store.Require(applicationID)
	if err != nil {
		return nil, err
	}
	targets, err := s.resolveChannels(cfg, channelIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]channel.ReleaseStage, len(targets))
	for _, ch := range targets {
		if stopAfter != nil {
			out[ch.ID()] = *stopAfter
		} else {
			out[ch.ID()] = ch.Capabilities().MaxStage()
		}
	}
	return out, nil
}

// MarketStates 查询应用在各渠道的状态。
//
// 单个渠道失败不影响其他渠道 —— 返回 map 而不是 error，让调用方逐个看结果。
func (s *Service) MarketStates(
	ctx context.Context,
	applicationID string,
	channelIDs []string,
	timeouts httpx.Timeouts,
) (map[string]MarketResult, error) {
	cfg, err := s.store.Require(applicationID)
	if err != nil {
		return nil, err
	}
	targets, err := s.resolveChannels(cfg, channelIDs)
	if err != nil {
		return nil, err
	}

	results := make(map[string]MarketResult, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ch := range targets {
		creds, err := s.credentialsFor(cfg, ch)
		if err != nil {
			results[ch.ID()] = MarketResult{Err: err}
			continue
		}
		wg.Add(1)
		go func(ch channel.Channel, creds channel.Credentials) {
			defer wg.Done()
			info, err := ch.QueryMarket(ctx, channel.MarketQuery{
				ApplicationID: cfg.ApplicationID,
				Credentials:   creds,
				Timeouts:      timeouts,
			})
			mu.Lock()
			results[ch.ID()] = MarketResult{Info: info, Err: err}
			mu.Unlock()
		}(ch, creds)
	}
	wg.Wait()
	return results, nil
}

// Submit 提交新版本。**立即返回**，不等待上传完成。
func (s *Service) Submit(ctx context.Context, opts SubmitOptions) (string, error) {
	cfg, err := s.store.Require(opts.ApplicationID)
	if err != nil {
		return "", err
	}
	targets, err := s.resolveChannels(cfg, opts.ChannelIDs)
	if err != nil {
		return "", err
	}
	if len(targets) == 0 {
		return "", eperr.ConfigurationError("没有启用任何渠道：%s", cfg.ApplicationID)
	}

	// 停留点是否被支持，必须在做任何实际工作之前判定。
	// 否则小米这类「无处可停」的渠道会先解析制品、查完市场状态，
	// 甚至开始上传，才告诉调用方这个请求根本不成立。
	if opts.StopAfter != nil {
		for _, ch := range targets {
			if err := channel.RequireSupportedStage(ch, *opts.StopAfter); err != nil {
				return "", err
			}
		}
	}

	plans := make([]channelPlan, 0, len(targets))
	for _, ch := range targets {
		stage := ch.Capabilities().MaxStage()
		if opts.StopAfter != nil {
			stage = *opts.StopAfter
		}
		path, err := locate(cfg, ch, opts.ArtifactPath)
		if err != nil {
			return "", err
		}
		info, err := s.readInfo(path)
		if err != nil {
			return "", err
		}
		creds, err := s.credentialsFor(cfg, ch)
		if err != nil {
			return "", err
		}
		plans = append(plans, channelPlan{ch: ch, path: path, info: info, creds: creds, stage: stage})
	}

	// 预解析：任何一个渠道的制品缺失或配置错误都应在返回 id 之前暴露，
	// 而不是留到后台执行时才失败 —— 否则调用方只能从轮询结果里发现参数写错了
	reference := plans[0].info
	if reference.ApplicationID != cfg.ApplicationID {
		return "", eperr.ConfigurationError(
			"制品的包名 %s 与配置的 %s 不一致", reference.ApplicationID, cfg.ApplicationID)
	}

	progress := make([]ChannelProgress, 0, len(plans))
	for _, p := range plans {
		progress = append(progress, ChannelProgress{
			ChannelID:   p.ch.ID(),
			DisplayName: p.ch.DisplayName(),
			Stage:       Stage{Kind: StageWaiting},
		})
	}

	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("job-%d-%d", time.Now().Unix(), s.seq)
	handle := &jobHandle{snapshot: Job{
		ID:             id,
		ApplicationID:  cfg.ApplicationID,
		ArtifactPath:   opts.ArtifactPath,
		VersionCode:    reference.VersionCode,
		VersionName:    reference.VersionName,
		Channels:       progress,
		RequestedStage: opts.StopAfter,
		StartedAt:      time.Now().UnixMilli(),
	}}
	s.jobs[id] = handle
	s.mu.Unlock()

	jobCtx, cancel := context.WithCancel(s.bg)
	handle.setCancel(cancel)
	// ctx 取消（例如请求方超时）时也要停掉后台任务
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-jobCtx.Done():
		}
	}()

	for _, p := range plans {
		go s.uploadOne(jobCtx, handle, p, opts.Release, opts.Version, opts.Timeouts)
	}
	return id, nil
}

// Job 返回任务快照。
func (s *Service) Job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return h.current(), true
}

// JobIDs 返回本进程内的任务 id。
func (s *Service) JobIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		out = append(out, id)
	}
	return out
}

// Cancel 取消任务。
func (s *Service) Cancel(id string) bool {
	s.mu.Lock()
	h, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	return h.doCancel()
}

// ---- 内部 ----

type channelPlan struct {
	ch    channel.Channel
	path  string
	info  artifact.Info
	creds channel.Credentials
	stage channel.ReleaseStage
}

type jobHandle struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	snapshot Job
	stages   map[string]Stage
}

// setCancel 与 doCancel 都走同一把锁：Submit 写入 cancel 与 Cancel 读取它
// 可能并发，裸字段访问会是数据竞争（go test -race 会报）。
func (h *jobHandle) setCancel(fn context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancel = fn
}

func (h *jobHandle) doCancel() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel == nil {
		return false
	}
	h.cancel()
	return true
}

func (h *jobHandle) current() Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.snapshot
	out.Channels = append([]ChannelProgress(nil), h.snapshot.Channels...)
	return out
}

func (h *jobHandle) update(channelID string, stage Stage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stages == nil {
		h.stages = map[string]Stage{}
	}
	h.stages[channelID] = stage
	for i := range h.snapshot.Channels {
		if st, ok := h.stages[h.snapshot.Channels[i].ChannelID]; ok {
			h.snapshot.Channels[i].Stage = st
		}
	}
	done := true
	for _, c := range h.snapshot.Channels {
		if !c.Stage.Terminal() {
			done = false
			break
		}
	}
	if done && h.snapshot.FinishedAt == 0 {
		h.snapshot.FinishedAt = time.Now().UnixMilli()
	}
}

func (s *Service) uploadOne(
	ctx context.Context,
	handle *jobHandle,
	plan channelPlan,
	release channel.ReleaseParams,
	rule VersionRule,
	timeouts httpx.Timeouts,
) {
	ch := plan.ch
	id := ch.ID()

	// 只上传制品时不做版本号校验：此时不产生任何版本，
	// 拦截只会妨碍「验证凭据与签名是否可用」这个用途
	if plan.stage != channel.StageUploadArtifact {
		handle.update(id, Stage{Kind: StageWorking, Action: "检查渠道状态"})
		var market *channel.MarketInfo
		if info, err := ch.QueryMarket(ctx, channel.MarketQuery{
			ApplicationID: plan.info.ApplicationID,
			Credentials:   plan.creds,
			Timeouts:      timeouts,
		}); err == nil {
			market = &info
		} else {
			// 状态查询失败不阻止提交尝试，让渠道自己拒绝并给出真实原因。
			// 鸿蒙渠道就是有意不支持状态查询的
			logx.Debug("渠道状态查询失败，跳过版本号比对", "channel", id, "err", err)
		}
		if err := Reject(plan.info, market, rule); err != nil {
			handle.update(id, StageOfError(err))
			logx.Error("不满足发布前置条件", "channel", id, "err", err)
			return
		}
	}

	handle.update(id, Stage{Kind: StageWorking, Action: "请求中"})
	reached, err := ch.Upload(ctx, channel.UploadRequest{
		ArtifactFile:  plan.path,
		ArtifactInfo:  plan.info,
		Credentials:   plan.creds,
		ReleaseParams: release,
		Timeouts:      timeouts,
		OnProgress: func(fraction float64) {
			handle.update(id, Stage{Kind: StageUploading, Fraction: fraction})
		},
		StopAfter: plan.stage,
	})

	switch {
	case err == nil:
		// 以渠道返回的实际阶段为准，不假设请求里的 StopAfter 已达成
		handle.update(id, Stage{Kind: StageSucceeded, Reached: reached})
		logx.Info("发布完成", "channel", ch.DisplayName(), "stage", reached.Label(),
			"artifact", plan.info.String())

	case isCanceled(err):
		// 取消不是失败。上游用 catch(Throwable) 把取消当成失败上报，
		// 界面显示「上传失败」并给出重试按钮，取消语义完全失效
		handle.update(id, Stage{Kind: StageCancelled})
		logx.Info("已取消", "channel", ch.DisplayName())

	default:
		handle.update(id, StageOfError(err))
		logx.Error("提交失败", "channel", ch.DisplayName(), "err", err)
	}
}

// isCanceled 区分「用户取消」与「真的失败」。
//
// 上游用 catch(Throwable) 把取消也当成失败上报，界面显示「上传失败」并给出
// 重试按钮，后续渠道逐个变成 Error，取消语义完全失效。
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) resolveChannels(cfg config.AppConfig, channelIDs []string) ([]channel.Channel, error) {
	if len(channelIDs) == 0 {
		var out []channel.Channel
		for _, enabled := range cfg.EnabledChannels() {
			for _, ch := range s.channels {
				if strings.EqualFold(ch.ID(), enabled.Name) {
					out = append(out, ch)
					break
				}
			}
		}
		return out, nil
	}
	out := make([]channel.Channel, 0, len(channelIDs))
	for _, id := range channelIDs {
		var found channel.Channel
		for _, ch := range s.channels {
			if strings.EqualFold(ch.ID(), id) {
				found = ch
				break
			}
		}
		if found == nil {
			ids := make([]string, 0, len(s.channels))
			for _, ch := range s.channels {
				ids = append(ids, ch.ID())
			}
			return nil, eperr.ConfigurationError(
				"未知渠道：%s（可用渠道：%s）", id, strings.Join(ids, ", "))
		}
		if _, ok := cfg.Channel(found.ID()); !ok {
			return nil, eperr.ConfigurationError(
				"应用 %s 未配置渠道 %s", cfg.ApplicationID, found.ID())
		}
		out = append(out, found)
	}
	return out, nil
}

// credentialsFor 组装渠道凭据。环境变量优先于配置文件，CI 场景可完全不落盘。
func (s *Service) credentialsFor(cfg config.AppConfig, ch channel.Channel) (channel.Credentials, error) {
	store := config.NewLayeredStore(cfg, nil)
	values := make(map[string]string, len(ch.Params()))
	for _, param := range ch.Params() {
		value, ok, err := store.Get(cfg.ApplicationID, ch.ID(), param.Name)
		if err != nil {
			return channel.Credentials{}, err
		}
		if !ok {
			if param.Required {
				return channel.Credentials{}, eperr.CredentialError(
					"渠道 %s 缺少必填参数 %s（%s）", ch.DisplayName(), param.Name, param.Description)
			}
			continue
		}
		values[param.Name] = value
	}
	return channel.NewCredentials(values), nil
}

// ---- 制品定位 ----

const locateMaxDepth = 3

// locate 为渠道定位对应的制品文件。
//
// 多渠道包场景下每个渠道一个包，靠文件名里的渠道标识匹配
// （如 app-HUAWEI-1.2.0.apk、app-HARMONY-1.2.0.app）。
func locate(cfg config.AppConfig, ch channel.Channel, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", eperr.LocalFileError("路径不存在：%s", path)
	}
	accepted := acceptedExtensions(ch)

	if !st.IsDir() {
		// 扩展名必须被该渠道接受。把 .apk 传给鸿蒙渠道这类错误应当立刻暴露，
		// 而不是等到上传了几百兆之后被服务端拒绝
		if !hasAcceptedExt(path, accepted) {
			return "", eperr.LocalFileError(
				"%s 渠道不接受 .%s 文件（%s），该渠道需要：%s",
				ch.DisplayName(), strings.TrimPrefix(filepath.Ext(path), "."),
				filepath.Base(path), strings.Join(prefixDots(accepted), "、"))
		}
		if cfg.MultiChannelApk && !matchesTag(path, ch) {
			return "", eperr.LocalFileError(
				"文件名 %s 不含渠道标识 %s，与 %s 渠道不匹配",
				filepath.Base(path), ch.FileNameTag(), ch.DisplayName())
		}
		return path, nil
	}

	var candidates []string
	_ = walkLimited(path, locateMaxDepth, func(p string, info os.FileInfo) {
		if !info.IsDir() && hasAcceptedExt(p, accepted) {
			candidates = append(candidates, p)
		}
	})
	if len(candidates) == 0 {
		return "", eperr.LocalFileError("目录下没有找到 %s 文件：%s",
			strings.Join(prefixDots(accepted), "、"), path)
	}

	var matched []string
	for _, c := range candidates {
		if matchesTag(c, ch) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", eperr.LocalFileError(
			"目录下没有找到文件名包含 %s 的制品（%s 渠道）：%s",
			ch.FileNameTag(), ch.DisplayName(), path)
	default:
		// 多个候选时取最新修改的。不报错是因为多渠道包目录里同一渠道出现多个
		// 历史版本很常见，取最新符合直觉；但会在日志里说明依据
		newest := matched[0]
		var newestTime time.Time
		for _, m := range matched {
			if info, err := os.Stat(m); err == nil && info.ModTime().After(newestTime) {
				newest, newestTime = m, info.ModTime()
			}
		}
		logx.Debug("目录下有多个匹配文件，取最新修改的",
			"channel", ch.ID(), "chosen", filepath.Base(newest), "candidates", len(matched))
		return newest, nil
	}
}

func acceptedExtensions(ch channel.Channel) []string {
	var out []string
	for _, kind := range ch.ArtifactKinds() {
		out = append(out, kind.Extensions()...)
	}
	if len(out) == 0 {
		out = artifact.KindAPK.Extensions()
	}
	return out
}

func hasAcceptedExt(path string, accepted []string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	for _, a := range accepted {
		if strings.EqualFold(ext, a) {
			return true
		}
	}
	return false
}

func matchesTag(path string, ch channel.Channel) bool {
	return strings.Contains(strings.ToLower(filepath.Base(path)), strings.ToLower(ch.FileNameTag()))
}

func prefixDots(exts []string) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		out = append(out, "."+e)
	}
	return out
}

// walkLimited 遍历目录，限制深度。
func walkLimited(root string, maxDepth int, fn func(string, os.FileInfo)) error {
	rootDepth := strings.Count(root, string(os.PathSeparator))
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个条目读不了不影响整体
		}
		if info.IsDir() && strings.Count(path, string(os.PathSeparator))-rootDepth >= maxDepth {
			return filepath.SkipDir
		}
		fn(path, info)
		return nil
	})
}
