package publish

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"os"
	"path/filepath"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

func testArtifact(versionCode int64) artifact.Info {
	return artifact.Info{
		Path:          "/tmp/app.apk",
		ApplicationID: testAppID,
		VersionCode:   versionCode,
		VersionName:   "1.0.0",
		SizeBytes:     1024,
		Kind:          artifact.KindAPK,
	}
}

func market(code int64, state channel.ReviewState) *channel.MarketInfo {
	info := channel.NewMarketInfo("huawei", state, &channel.Version{Code: code, Name: "1.0.0"}, "")
	return &info
}

const testAppID = "com.example.app"

func TestRejectVersionRules(t *testing.T) {
	cases := []struct {
		name     string
		incoming int64
		online   *channel.MarketInfo
		rule     VersionRule
		wantErr  bool
	}{
		{"版本号更高时允许", 11, market(10, channel.ReviewOnline), VersionStrict, false},
		{"版本号相同时默认拒绝", 10, market(10, channel.ReviewOnline), VersionStrict, true},
		{"版本号相同时 AllowSame 放行", 10, market(10, channel.ReviewOnline), VersionAllowSame, false},
		// 与上游那个 PR 的关键差别：它跳过整个判断，连低于线上版本也放行，
		// 而版本号变低几乎总是选错了制品文件
		{"版本号更低时即使 AllowSame 也拒绝", 9, market(10, channel.ReviewOnline), VersionAllowSame, true},
		{"Skip 跳过全部版本号校验", 1, market(10, channel.ReviewOnline), VersionSkip, false},
		// 应用在商店只有未上传 APK 的草稿版本时渠道不返回版本号，
		// 这正是上游 issue #7 的场景
		{"线上版本未知时不阻断", 1, market(0, channel.ReviewOnline), VersionStrict, false},
		{"没有渠道状态时不阻断", 1, nil, VersionStrict, false},
		{"渠道声明不可提交时拒绝", 11, unavailable(), VersionStrict, true},
		{"审核中时拒绝", 11, market(10, channel.ReviewUnderReview), VersionStrict, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Reject(testArtifact(tc.incoming), tc.online, tc.rule)
			if tc.wantErr && err == nil {
				t.Fatal("期望被拒绝，实际放行")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望放行，实际被拒绝：%v", err)
			}
			if err != nil && err.Kind != eperr.KindPrecondition {
				t.Errorf("Kind = %v, 期望 Precondition", err.Kind)
			}
		})
	}
}

func unavailable() *channel.MarketInfo {
	info := channel.NewMarketInfo("huawei", channel.ReviewOnline,
		&channel.Version{Code: 10, Name: "1.0.0"}, "")
	info.CanSubmit = false
	return &info
}

func TestRejectKeepsRawState(t *testing.T) {
	info := channel.NewMarketInfo("huawei", channel.ReviewUnknown, nil, "99")
	info.CanSubmit = false
	err := Reject(testArtifact(11), &info, VersionStrict)
	if err == nil {
		t.Fatal("期望被拒绝")
	}
	// 华为将来新增状态码时，至少要能把原始值交到用户手里
	if !strings.Contains(err.Msg, "99") {
		t.Errorf("错误信息应包含原始状态值，实际：%s", err.Msg)
	}
}

// ---- 编排 ----

type fakeChannel struct {
	id   string
	caps channel.Capabilities

	// reached 非 nil 时 upload 返回它，否则返回请求的 StopAfter
	reached *channel.ReleaseStage
	failErr error
	market  *channel.MarketInfo

	mu            sync.Mutex
	uploadCalls   int
	marketQueries int
	lastStopAfter channel.ReleaseStage
	gotTimeouts   httpx.Timeouts
}

func (f *fakeChannel) ID() string          { return f.id }
func (f *fakeChannel) DisplayName() string { return f.id }
func (f *fakeChannel) FileNameTag() string { return strings.ToUpper(f.id) }
func (f *fakeChannel) Params() []channel.ChannelParam {
	return nil // 不需要凭据，便于测试
}
func (f *fakeChannel) Capabilities() channel.Capabilities {
	if f.caps.SupportedStages == nil {
		return channel.Capabilities{
			SupportedStages: channel.AllStages(),
			RiskLevel:       channel.RiskMedium,
			Withdrawal:      channel.WithdrawalNotVerified,
			Evidence:        channel.EvidenceCodeObservation,
			Note:            "测试用假渠道",
		}
	}
	return f.caps
}
func (f *fakeChannel) ArtifactKinds() []artifact.Kind { return []artifact.Kind{artifact.KindAPK} }

func (f *fakeChannel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	f.mu.Lock()
	f.uploadCalls++
	f.lastStopAfter = req.StopAfter
	f.gotTimeouts = req.Timeouts
	f.mu.Unlock()

	if f.failErr != nil {
		return 0, f.failErr
	}
	if f.reached != nil {
		return *f.reached, nil
	}
	return req.StopAfter, nil
}

func (f *fakeChannel) QueryMarket(ctx context.Context, q channel.MarketQuery) (channel.MarketInfo, error) {
	f.mu.Lock()
	f.marketQueries++
	f.mu.Unlock()
	if f.market == nil {
		return channel.MarketInfo{}, eperr.ProtocolError(f.id, "无状态可查")
	}
	return *f.market, nil
}

func (f *fakeChannel) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploadCalls, f.marketQueries
}

// newTestService 构造服务，并返回一个真实存在的制品文件路径。
//
// 必须是真的文件：Submit 里的 locate 会 stat 路径并按扩展名筛选，
// 这一步在注入的 artifact reader 之前，假路径过不去。
func newTestService(t *testing.T, cfg config.AppConfig, channels []channel.Channel,
	reader func(string) (artifact.Info, error)) (*Service, string) {
	t.Helper()
	store := config.NewStore(t.TempDir())
	if err := store.Save(cfg); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	apkPath := filepath.Join(t.TempDir(), "app.apk")
	if err := os.WriteFile(apkPath, []byte("not really an apk"), 0o600); err != nil {
		t.Fatalf("创建测试制品失败: %v", err)
	}
	if reader == nil {
		reader = func(string) (artifact.Info, error) { return testArtifact(10), nil }
	}
	return NewService(store, WithChannels(channels), WithArtifactReader(reader)), apkPath
}

func testConfig(channelIDs ...string) config.AppConfig {
	channels := make([]config.ChannelConfig, 0, len(channelIDs))
	for _, id := range channelIDs {
		channels = append(channels, config.ChannelConfig{Name: id, Enabled: true})
	}
	return config.AppConfig{
		Name:          "测试应用",
		ApplicationID: testAppID,
		Channels:      channels,
	}
}

// waitForJob 轮询直到任务结束。
func waitForJob(t *testing.T, s *Service, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := s.Job(id)
		if ok && job.Done() {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 超时未结束", id)
	return Job{}
}

func TestSubmitRecordsActualStageNotRequested(t *testing.T) {
	reached := channel.StageUploadArtifact
	ch := &fakeChannel{id: "huawei", reached: &reached}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	// 渠道声称只走到上传，即使请求的是送审 —— service 必须如实记录
	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, s, id)

	stage := job.Channels[0].Stage
	if stage.Kind != StageSucceeded {
		t.Fatalf("Kind = %v, 期望 Succeeded（%s）", stage.Kind, stage.Label())
	}
	if stage.Reached != channel.StageUploadArtifact {
		t.Errorf("Reached = %v, 期望 UploadArtifact", stage.Reached)
	}
	if stage.Label() != "已上传安装包（未创建版本）" {
		t.Errorf("Label = %q", stage.Label())
	}
}

func TestSubmitDraftNotReportedAsSubmitted(t *testing.T) {
	ch := &fakeChannel{id: "honor"}
	s, apkPath := newTestService(t, testConfig("honor"), []channel.Channel{ch}, nil)

	draft := channel.StageCreateDraft
	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		StopAfter:     &draft,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, s, id)

	if job.RequestedStage == nil || *job.RequestedStage != channel.StageCreateDraft {
		t.Errorf("RequestedStage = %v", job.RequestedStage)
	}
	if ch.lastStopAfter != channel.StageCreateDraft {
		t.Errorf("传给渠道的 StopAfter = %v", ch.lastStopAfter)
	}
	stage := job.Channels[0].Stage
	if stage.Label() != "草稿已就绪（未送审）" {
		t.Errorf("Label = %q，停在草稿态不能显示为已提交", stage.Label())
	}
}

func TestUnspecifiedStopAfterResolvesPerChannel(t *testing.T) {
	// 华为能送审，鸿蒙只能到草稿 —— 不指定停留点时各走各的最远阶段
	huawei := &fakeChannel{id: "huawei"}
	mi := &fakeChannel{id: "mi", caps: channel.Capabilities{
		SupportedStages: []channel.ReleaseStage{channel.StageSubmitReview},
		RiskLevel:       channel.RiskCritical,
		Note:            "dev/push 原子请求",
	}}
	// 这个测试只调 ResolvedStages，不会走到制品定位，因此不需要路径
	s, _ := newTestService(t, testConfig("huawei", "mi"), []channel.Channel{huawei, mi}, nil)

	stages, err := s.ResolvedStages(testAppID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stages["huawei"] != channel.StageSubmitReview {
		t.Errorf("huawei 解析为 %v, 期望 SubmitReview", stages["huawei"])
	}
	if stages["mi"] != channel.StageSubmitReview {
		t.Errorf("mi 解析为 %v", stages["mi"])
	}

	draft := channel.StageCreateDraft
	stages, err = s.ResolvedStages(testAppID, nil, &draft)
	if err != nil {
		t.Fatal(err)
	}
	if stages["huawei"] != channel.StageCreateDraft {
		t.Errorf("显式指定时应原样返回，得到 %v", stages["huawei"])
	}
}

func TestUploadArtifactSkipsVersionCheck(t *testing.T) {
	ch := &fakeChannel{id: "huawei", market: market(100, channel.ReviewOnline)}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	// 线上版本 100 高于待提交的 10，正常送审会被拦；但只上传文件不产生版本，不该拦
	artifactStage := channel.StageUploadArtifact
	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		StopAfter:     &artifactStage,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, s, id)

	if job.Channels[0].Stage.Kind != StageSucceeded {
		t.Errorf("仅上传制品不应被版本号校验拦住，实际：%s", job.Channels[0].Stage.Label())
	}
	if _, queries := ch.counts(); queries != 0 {
		t.Errorf("不该去查渠道状态，实际查了 %d 次", queries)
	}
}

func TestDraftStillChecksVersion(t *testing.T) {
	ch := &fakeChannel{id: "huawei", market: market(100, channel.ReviewOnline)}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	draft := channel.StageCreateDraft
	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		StopAfter:     &draft,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, s, id)

	stage := job.Channels[0].Stage
	if stage.Kind != StageFailed {
		t.Fatalf("草稿也是要建版本的，版本号过低应当被拦，实际：%s", stage.Label())
	}
	if stage.ErrKind != eperr.KindPrecondition {
		t.Errorf("Kind = %v", stage.ErrKind)
	}
	if calls, _ := ch.counts(); calls != 0 {
		t.Errorf("校验不通过就不该上传，实际调用 %d 次", calls)
	}
}

func TestSingleChannelFailureDoesNotBlockOthers(t *testing.T) {
	failing := &fakeChannel{id: "mi", failErr: eperr.RejectedError("mi", "131004", "版本号已存在")}
	ok := &fakeChannel{id: "huawei"}
	s, apkPath := newTestService(t, testConfig("huawei", "mi"), []channel.Channel{failing, ok}, nil)

	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	job := waitForJob(t, s, id)

	if job.State() != JobPartiallyFailed {
		t.Errorf("State = %v, 期望 PartiallyFailed", job.State())
	}
	if got := job.Succeeded(); len(got) != 1 || got[0] != "huawei" {
		t.Errorf("Succeeded = %v", got)
	}
	if got := job.Failed(); len(got) != 1 || got[0].ChannelID != "mi" {
		t.Errorf("Failed = %v", got)
	}
	if calls, _ := ok.counts(); calls != 1 {
		t.Errorf("成功的渠道应当照常上传，实际 %d 次", calls)
	}
}

func TestSubmissionPointFailureIsNotRetryable(t *testing.T) {
	// 模拟「服务端已受理但响应丢失」：渠道内部已用 AtSubmissionPoint 标记
	netErr := &net.OpError{Op: "read", Err: errors.New("i/o timeout")}
	tagged := eperr.NetworkError("huawei", netErr).AtSubmissionPoint("华为", "提交审核")
	ch := &fakeChannel{id: "huawei", failErr: tagged}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := waitForJob(t, s, id).Channels[0].Stage

	if stage.Kind != StageFailed {
		t.Fatalf("Kind = %v", stage.Kind)
	}
	if stage.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v", stage.Phase)
	}
	if stage.Retryable {
		t.Error("送审后的网络失败绝不能标记为可重试")
	}
	if !strings.Contains(stage.Message, "确认") {
		t.Errorf("提示应引导先确认：%s", stage.Message)
	}
	if stage.Summary() != "失败（网络失败，已送审需人工确认）" {
		t.Errorf("Summary = %q", stage.Summary())
	}
}

func TestPreSubmissionNetworkFailureIsRetryable(t *testing.T) {
	netErr := &net.OpError{Op: "read", Err: errors.New("i/o timeout")}
	ch := &fakeChannel{id: "huawei", failErr: eperr.NetworkError("huawei", netErr)}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := waitForJob(t, s, id).Channels[0].Stage

	if stage.Phase != eperr.PhasePreSubmission {
		t.Errorf("Phase = %v", stage.Phase)
	}
	if !stage.Retryable {
		t.Error("送审前的网络失败应当可重试")
	}
}

func TestUnsupportedStageFailsBeforeAnyWork(t *testing.T) {
	// 小米的 dev/push 是原子的，无处可停。这个校验必须早于制品解析与状态查询，
	// 否则会先白做一堆工作，甚至在某些实现里开始上传，才告诉调用方请求不成立
	mi := &fakeChannel{id: "mi", caps: channel.Capabilities{
		SupportedStages: []channel.ReleaseStage{channel.StageSubmitReview},
		RiskLevel:       channel.RiskCritical,
		Note:            "dev/push 原子请求，无处可停",
	}}
	readerCalled := false
	s, apkPath := newTestService(t, testConfig("mi"), []channel.Channel{mi},
		func(string) (artifact.Info, error) {
			readerCalled = true
			return testArtifact(10), nil
		})

	draft := channel.StageCreateDraft
	_, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		StopAfter:     &draft,
	})
	if err == nil {
		t.Fatal("不支持的停留点应当立刻报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration 错误，实际 %v", err)
	}
	if readerCalled {
		t.Error("不该解析制品")
	}
	if calls, queries := mi.counts(); calls != 0 || queries != 0 {
		t.Errorf("不该发起任何调用，实际 upload=%d query=%d", calls, queries)
	}
	if !strings.Contains(err.Error(), "草稿") {
		t.Errorf("应当告知可停在哪些阶段：%v", err)
	}
}

func TestPackageNameMismatchFailsBeforeJobID(t *testing.T) {
	ch := &fakeChannel{id: "huawei"}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch},
		func(string) (artifact.Info, error) {
			info := testArtifact(10)
			info.ApplicationID = "com.other.app"
			return info, nil
		})

	_, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err == nil {
		t.Fatal("包名不一致应当报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration 错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "com.other.app") {
		t.Errorf("应说明实际包名：%v", err)
	}
}

func TestUnconfiguredChannelFails(t *testing.T) {
	ch := &fakeChannel{id: "huawei"}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	_, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		ChannelIDs:    []string{"vivo"},
	})
	if err == nil {
		t.Fatal("未配置的渠道应当报错")
	}
}

func TestCancelMarksCancelledNotFailed(t *testing.T) {
	// 取消必须被识别为取消，不能显示成失败并提示重试 ——
	// 上游用 catch(Throwable) 把两者混为一谈
	block := make(chan struct{})
	// 只在测试结束时兜底解除阻塞。不能在 Cancel 之后立刻 close ——
	// 那样 select 的两个分支同时就绪，Go 会随机选一个，测试就变成抛硬币
	defer close(block)

	slow := &blockingChannel{fakeChannel: fakeChannel{id: "huawei"}, block: block}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{slow}, nil)

	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 等上传真的开始，否则可能在 goroutine 启动前就取消，测不到中断在途请求的路径
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job, ok := s.Job(id); ok && job.Channels[0].Stage.Kind != StageWaiting {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if !s.Cancel(id) {
		t.Fatal("Cancel 应返回 true")
	}
	job := waitForJob(t, s, id)
	stage := job.Channels[0].Stage
	if stage.Kind != StageCancelled {
		t.Errorf("Kind = %v（%s），期望 Cancelled", stage.Kind, stage.Label())
	}
	if job.State() != JobCancelled {
		t.Errorf("State = %v", job.State())
	}
}

// blockingChannel 在 Upload 里阻塞，直到 block 关闭或 ctx 取消。
type blockingChannel struct {
	fakeChannel
	block chan struct{}
}

func (b *blockingChannel) Upload(ctx context.Context, req channel.UploadRequest) (channel.ReleaseStage, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-b.block:
		return req.StopAfter, nil
	}
}

func TestTimeoutsAreThreadedThrough(t *testing.T) {
	ch := &fakeChannel{id: "huawei"}
	s, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch}, nil)

	timeouts := httpx.OfSeconds(300)
	id, err := s.Submit(context.Background(), SubmitOptions{
		ApplicationID: testAppID,
		ArtifactPath:  apkPath,
		Timeouts:      timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, id)
	if ch.gotTimeouts.Read != 300*time.Second {
		t.Errorf("传给渠道的超时 = %v, 期望 300s", ch.gotTimeouts.Read)
	}
}
