package huawei

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// ---- 脚手架 ----

type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

type fakeHuawei struct {
	mu        sync.Mutex
	responses map[string]string
	requests  []recorded
}

// set 设置某路径的响应。必须走锁：测试线程写、服务端 goroutine 读。
func (f *fakeHuawei) set(path, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = resp
}

func (f *fakeHuawei) record(r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recorded{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   body,
	})
}

func (f *fakeHuawei) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeHuawei) paths() []string {
	out := []string{}
	for _, r := range f.all() {
		out = append(out, r.Path)
	}
	return out
}

func (f *fakeHuawei) count(suffix string) int {
	n := 0
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

func (f *fakeHuawei) mustFind(t *testing.T, suffix string) recorded {
	t.Helper()
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			return r
		}
	}
	t.Fatalf("没找到路径以 %q 结尾的请求，实际：%v", suffix, f.paths())
	return recorded{}
}

const uploadPath = "/obs-upload"

// newFakeHuawei 起假网关并返回渠道实例。
//
// 轮询间隔压到 1ms，否则测试要真等 10 秒一轮。
func newFakeHuawei(t *testing.T) (*Channel, *fakeHuawei) {
	t.Helper()
	fake := &fakeHuawei{responses: map[string]string{}}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.record(r, string(body))

		fake.mu.Lock()
		resp, ok := fake.responses[r.URL.Path]
		fake.mu.Unlock()
		if !ok {
			resp, ok = defaults[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ret":{"code":404,"msg":"unexpected ` + r.URL.Path + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	ch := NewWithBaseURL(srv.URL + "/")
	ch.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }
	ch.pollTimeouts = &pollConfig{interval: time.Millisecond, timeout: 2 * time.Second}

	// 上传地址指向同一个假网关，并带上华为会下发的签名头
	fake.set("/api/publish/v2/upload-url/for-obs",
		`{"ret":{"code":0},"urlInfo":{"url":"`+srv.URL+uploadPath+`",`+
			`"objectId":"obj-9001","headers":{"x-obs-signature":"sig-abc"}}}`)
	fake.set(uploadPath, ``)

	return ch, fake
}

// defaults 是各接口的默认成功响应。
var defaults = map[string]string{
	"/api/oauth2/v1/token": `{"access_token":"tok-huawei"}`,
	"/api/publish/v2/appid-list": `{"ret":{"code":0},"appids":[` +
		`{"key":"com.example.app","value":"app-777"}]}`,
	"/api/publish/v2/app-info": `{"ret":{"code":0},"appInfo":{` +
		`"releaseState":0,"versionCode":1000,"versionNumber":"1.0.0"}}`,
	"/api/publish/v2/app-file-info": `{"ret":{"code":0},"pkgVersion":["pkg-555"]}`,
	"/api/publish/v2/package/compile/status": `{"ret":{"code":0},` +
		`"pkgStateList":[{"pkgId":"pkg-555","successStatus":0}]}`,
	"/api/publish/v2/app-language-info": `{"ret":{"code":0}}`,
	"/api/publish/v2/app-submit":        `{"ret":{"code":0}}`,
}

func testArtifactFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-HUAWEI-1.0.0.apk")
	if err := os.WriteFile(path, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRequest(t *testing.T, path string, stage channel.ReleaseStage) channel.UploadRequest {
	t.Helper()
	return channel.UploadRequest{
		ArtifactFile: path,
		ArtifactInfo: artifact.Info{
			Path: path, ApplicationID: "com.example.app",
			VersionCode: 1020, VersionName: "1.2.0", Kind: artifact.KindAPK,
		},
		Credentials: channel.NewCredentials(map[string]string{
			ParamClientID:     "test-client-id",
			ParamClientSecret: "test-client-secret",
		}),
		ReleaseParams: channel.ReleaseParams{UpdateDesc: "修复若干问题"},
		Timeouts:      httpx.Default(),
		StopAfter:     stage,
	}
}

// ---- 全流程 ----

func TestUploadFullFlowCallOrder(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)

	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	want := []string{
		"/api/oauth2/v1/token",
		"/api/publish/v2/appid-list",
		"/api/publish/v2/upload-url/for-obs",
		uploadPath,
		"/api/publish/v2/app-file-info",
		"/api/publish/v2/package/compile/status",
		"/api/publish/v2/app-language-info",
		"/api/publish/v2/app-submit",
	}
	got := fake.paths()
	if len(got) != len(want) {
		t.Fatalf("请求次数 = %d, 期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 步 = %q, 期望 %q\n完整顺序：%v", i+1, got[i], want[i], got)
		}
	}
}

func TestTokenRequestIsJSONBody(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	req := fake.mustFind(t, "/token")
	if req.Method != http.MethodPost {
		t.Errorf("方法 = %s, 期望 POST", req.Method)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, 华为 token 用 JSON body", ct)
	}
	var payload tokenReq
	if err := json.Unmarshal([]byte(req.Body), &payload); err != nil {
		t.Fatalf("请求体不是 JSON：%q", req.Body)
	}
	if payload.ClientID != "test-client-id" || payload.ClientSecret != "test-client-secret" ||
		payload.GrantType != "client_credentials" {
		t.Errorf("token 请求体不对：%+v", payload)
	}
	// token 请求自身不带鉴权头
	if req.Header.Get("client_id") != "" || req.Header.Get("Authorization") != "" {
		t.Error("token 请求不应带鉴权头")
	}
}

func TestBusinessRequestsCarryAuthHeaders(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	for _, r := range fake.all() {
		if strings.HasSuffix(r.Path, "/token") || r.Path == uploadPath {
			continue // token 不需要鉴权；上传用的是华为下发的签名头
		}
		if got := r.Header.Get("client_id"); got != "test-client-id" {
			t.Errorf("%s 的 client_id 头 = %q", r.Path, got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-huawei" {
			t.Errorf("%s 的 Authorization 头 = %q", r.Path, got)
		}
	}
}

func TestUploadPassesThroughOBSHeaders(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	upload := fake.mustFind(t, uploadPath)
	if upload.Method != http.MethodPut {
		t.Errorf("上传方法 = %s, 期望 PUT", upload.Method)
	}
	if ct := upload.Header.Get("Content-Type"); ct != APKMediaType {
		t.Errorf("Content-Type = %q, 期望 %q", ct, APKMediaType)
	}
	// 华为下发的签名头必须原样透传，增删都会导致 OBS 拒绝
	if got := upload.Header.Get("x-obs-signature"); got != "sig-abc" {
		t.Errorf("应原样透传华为下发的 headers，实际 x-obs-signature = %q", got)
	}
}

func TestBindApkSendsFileType5(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	bind := fake.mustFind(t, "app-file-info")
	if bind.Method != http.MethodPut {
		t.Errorf("绑定方法 = %s, 期望 PUT", bind.Method)
	}
	var payload refreshApk
	if err := json.Unmarshal([]byte(bind.Body), &payload); err != nil {
		t.Fatalf("请求体解析失败：%q", bind.Body)
	}
	// 5 = APK 文件类型，华为接口定义的固定值
	if payload.FileType != APKFileType {
		t.Errorf("fileType = %d, 期望 %d", payload.FileType, APKFileType)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files 长度 = %d", len(payload.Files))
	}
	if payload.Files[0].FileDestURL != "obj-9001" {
		t.Errorf("fileDestUrl = %q, 应当是上传返回的 objectId", payload.Files[0].FileDestURL)
	}
	if payload.Files[0].FileName != "app-HUAWEI-1.0.0.apk" {
		t.Errorf("fileName = %q", payload.Files[0].FileName)
	}
}

// ---- 轮询（含「先查再等」的修正）----

func TestCompilePollingChecksFirstThenWaits(t *testing.T) {
	// 前两次返回「编译中」，第三次成功
	attempts := 0
	_, ch := newSequencedHuawei(t, func(n int) string {
		attempts = n
		if n < 3 {
			return `{"ret":{"code":0},"pkgStateList":[{"pkgId":"pkg-555","successStatus":1}]}`
		}
		return `{"ret":{"code":0},"pkgStateList":[{"pkgId":"pkg-555","successStatus":0}]}`
	})

	// 间隔故意设大，让「先查」与「先等」的耗时可区分：
	//   先查再等：check, wait, check, wait, check  → 2 个间隔
	//   先等再查：wait, check, wait, check, wait, check → 3 个间隔
	// 若把间隔压到 1ms，两者只差 1ms，测试将无法失败 —— 等于没测
	const interval = 150 * time.Millisecond
	ch.pollTimeouts = &pollConfig{interval: interval, timeout: 5 * time.Second}

	path := testArtifactFile(t)
	start := time.Now()
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	elapsed := time.Since(start)

	if attempts != 3 {
		t.Errorf("轮询次数 = %d, 期望 3（第三次才成功）", attempts)
	}
	// 上游把 delay 放在检查之前，首次检查必然空等一个间隔（生产环境 10 秒）。
	// 3 次检查、2 个间隔是正确行为；3 个间隔说明首次检查被推迟了
	if elapsed >= 3*interval {
		t.Errorf("耗时 %v ≥ 3×间隔(%v)，首次检查没有立即执行 —— "+
			"「先查再等」的修正没生效", elapsed, interval)
	}
	if elapsed < 2*interval {
		t.Errorf("耗时 %v < 2×间隔(%v)，检查之间似乎没有等待", elapsed, interval)
	}
}

func TestCompileEmptyStateKeepsWaiting(t *testing.T) {
	// 华为返回空 pkgStateList 表示「尚未拿到状态」，应继续等待而不是抛异常。
	// 上游用 first()，空数组会抛 NoSuchElementException
	_, ch := newSequencedHuawei(t, func(n int) string {
		if n < 2 {
			return `{"ret":{"code":0},"pkgStateList":[]}`
		}
		return `{"ret":{"code":0},"pkgStateList":[{"pkgId":"p","successStatus":0}]}`
	})

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("空 pkgStateList 应继续等待而非报错：%v", err)
	}
}

func TestCompileTimeoutGivesActionableError(t *testing.T) {
	// 编译状态永远返回「进行中」，超时压到极短
	_, ch := newSequencedHuawei(t, func(int) string {
		return `{"ret":{"code":0},"pkgStateList":[{"pkgId":"p","successStatus":1}]}`
	})
	ch.pollTimeouts = &pollConfig{interval: time.Millisecond, timeout: 20 * time.Millisecond}

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft))
	if err == nil {
		t.Fatal("编译一直不成功应当超时报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindNetwork {
		t.Fatalf("期望 Network 类错误，实际 %v", err)
	}
	if !strings.Contains(pe.Msg, "后台确认") {
		t.Errorf("应给出下一步动作：%s", pe.Msg)
	}
}

// ---- issue #7：草稿态 ----

// 上游 issue #7：应用有一个尚未上传 APK 的草稿版本时（releaseState=7），
// 华为不返回 versionCode。原实现把该字段声明为非空，直接抛
// JsonDataException，用户连查询市场状态都做不了。原作者已确认是 bug。
func TestQueryMarketDraftStateWithoutVersionCode(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/app-info", `{"ret":{"code":0},"appInfo":{`+
		`"releaseState":7,"versionNumber":"1.2.0"}}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatalf("草稿态缺少 versionCode 时不应报错：%v", err)
	}
	if info.ReviewState != channel.ReviewDraft {
		t.Errorf("ReviewState = %v, releaseState=7 是草稿态", info.ReviewState)
	}
	// 不伪造版本号：0 会让版本号比对得出「待提交版本更高」的错误结论
	if info.LastVersion != nil {
		t.Errorf("LastVersion = %v, 缺少 versionCode 时应为 nil", info.LastVersion)
	}
	if info.RawState != "7" {
		t.Errorf("RawState = %q, 应保留原始状态值", info.RawState)
	}
}

func TestQueryMarketMissingAppInfo(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/app-info", `{"ret":{"code":0}}`)

	_, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err == nil {
		t.Fatal("缺少 appInfo 应当报错")
	}
	if !strings.Contains(err.Error(), "appInfo") {
		t.Errorf("应说明缺的是 appInfo：%v", err)
	}
}

func TestQueryMarketReleaseStateMapping(t *testing.T) {
	cases := []struct {
		state int
		want  channel.ReviewState
	}{
		{0, channel.ReviewOnline},
		{4, channel.ReviewUnderReview},
		{5, channel.ReviewUnderReview},
		{7, channel.ReviewDraft},
		{8, channel.ReviewRejected},
		{2, channel.ReviewOffline},
		{6, channel.ReviewOffline},
		{10, channel.ReviewOffline},
		{99, channel.ReviewUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.state), func(t *testing.T) {
			ch, fake := newFakeHuawei(t)
			fake.set("/api/publish/v2/app-info", fmt.Sprintf(
				`{"ret":{"code":0},"appInfo":{"releaseState":%d,"versionCode":1000,"versionNumber":"1.0.0"}}`,
				tc.state))

			info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
				ApplicationID: "com.example.app",
				Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
				Timeouts:      httpx.Default(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.ReviewState != tc.want {
				t.Errorf("releaseState=%d 映射为 %v, 期望 %v", tc.state, info.ReviewState, tc.want)
			}
		})
	}
}

// ---- 送审与格式 ----

func TestScheduledReleaseTimeHasNoColonInOffset(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	req := testRequest(t, path, channel.StageSubmitReview)
	req.ReleaseParams.OnlineTime = time.Date(2026, 1, 15, 10, 0, 0, 0, time.Local).UnixMilli()

	if _, err := ch.Upload(context.Background(), req); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "app-submit")
	got := submit.Query.Get("releaseTime")
	if got == "" {
		t.Fatal("定时发布应带 releaseTime")
	}
	if !strings.HasPrefix(got, "2026-01-15T10:00:00") {
		t.Errorf("releaseTime = %q", got)
	}
	// 华为要 +0800 而不是 +08:00。Go 的 "Z07:00" 布局会产出带冒号的形式
	if strings.HasSuffix(got, "+08:00") {
		t.Errorf("releaseTime = %q 用了带冒号的时区偏移，华为要求 +0800", got)
	}
	if !strings.HasSuffix(got, "+0800") {
		t.Errorf("releaseTime = %q, 期望以 +0800 结尾", got)
	}
}

func TestImmediateReleaseOmitsReleaseTime(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "app-submit")
	// 为空等价于「审核通过后立即发布」
	if got := submit.Query.Get("releaseTime"); got != "" {
		t.Errorf("立即发布不应带 releaseTime，实际 %q", got)
	}
}

func TestUpdateVersionDescUsesZhCN(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	desc := fake.mustFind(t, "app-language-info")
	var payload versionDesc
	if err := json.Unmarshal([]byte(desc.Body), &payload); err != nil {
		t.Fatalf("请求体解析失败：%q", desc.Body)
	}
	if payload.NewFeatures != "修复若干问题" {
		t.Errorf("newFeatures = %q", payload.NewFeatures)
	}
	if payload.Lang != DefaultLang {
		t.Errorf("lang = %q, 期望 %q", payload.Lang, DefaultLang)
	}
}

func TestSubmitFailureIsMarkedAtSubmissionPoint(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/app-submit", `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v，送审点的失败必须标记", pe.Phase)
	}
	if pe.Retryable() {
		t.Error("送审点的失败绝不能标记为可重试")
	}
	if !strings.Contains(pe.Msg, "确认") {
		t.Errorf("提示应引导先到后台确认：%s", pe.Msg)
	}
}

func TestPreSubmitFailureStaysRetryable(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/appid-list", `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Phase != eperr.PhasePreSubmission {
		t.Errorf("送审前的失败应保持 PreSubmission，实际 %v", err)
	}
}

func TestUploadAddressMustBeHTTPS(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/upload-url/for-obs",
		`{"ret":{"code":0},"urlInfo":{"url":"http://insecure.example.com/up","objectId":"o"}}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err == nil {
		t.Fatal("http 上传地址应当被拒绝")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("应说明拒绝原因：%v", err)
	}
}

func TestChannelRejectedSurfacesCode(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	fake.set("/api/publish/v2/app-submit",
		`{"ret":{"code":204144660,"msg":"[cds]submit failed, sensitivePermissionIconUrl is necessary !"}}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindChannelRejected {
		t.Fatalf("期望 ChannelRejected，实际 %v", err)
	}
	if pe.Code != "204144660" {
		t.Errorf("Code = %q", pe.Code)
	}
	// 错误码要出现在 message 里：用户拿它去查华为文档
	if !strings.Contains(pe.Msg, "code=204144660") {
		t.Errorf("message 应带上错误码：%s", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "sensitivePermissionIconUrl") {
		t.Errorf("应保留渠道的原始描述：%s", pe.Msg)
	}
}

// ---- 停留点 ----

func TestStopAtArtifactSkipsBind(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageUploadArtifact {
		t.Errorf("reached = %v", reached)
	}
	for _, p := range fake.paths() {
		switch {
		case strings.HasSuffix(p, "app-file-info"):
			t.Error("停在仅上传时不应绑定文件")
		case strings.HasSuffix(p, "compile/status"):
			t.Error("停在仅上传时不应轮询编译状态")
		case strings.HasSuffix(p, "app-language-info"):
			t.Error("停在仅上传时不应改描述")
		case strings.HasSuffix(p, "app-submit"):
			t.Error("停在仅上传时不应送审")
		}
	}
}

func TestStopAtDraftRequiresBindAndCompile(t *testing.T) {
	ch, fake := newFakeHuawei(t)
	path := testArtifactFile(t)
	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageCreateDraft {
		t.Errorf("reached = %v", reached)
	}

	// 草稿就绪的定义是「已绑定 + 编译通过 + 描述已写入」，三者缺一不可
	var bound, compiled, described bool
	for _, p := range fake.paths() {
		switch {
		case strings.HasSuffix(p, "app-file-info"):
			bound = true
		case strings.HasSuffix(p, "compile/status"):
			compiled = true
		case strings.HasSuffix(p, "app-language-info"):
			described = true
		case strings.HasSuffix(p, "app-submit"):
			t.Error("停在草稿态时不应送审")
		}
	}
	if !bound || !compiled || !described {
		t.Errorf("草稿态需要绑定(%v)+编译通过(%v)+写描述(%v)", bound, compiled, described)
	}
}

// ---- 声明 ----

func TestCapabilitiesDeclaration(t *testing.T) {
	caps := New().Capabilities()
	want := []channel.ReleaseStage{
		channel.StageUploadArtifact, channel.StageCreateDraft, channel.StageSubmitReview,
	}
	if len(caps.SupportedStages) != len(want) {
		t.Fatalf("SupportedStages = %v", caps.SupportedStages)
	}
	for i := range want {
		if caps.SupportedStages[i] != want[i] {
			t.Errorf("SupportedStages[%d] = %v", i, caps.SupportedStages[i])
		}
	}
	if caps.RiskLevel != channel.RiskHigh {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	// 华为 AGC 文档里有「撤销审核」接口，平台层面支持撤回 ——
	// 这一点与上游 issue #16 的结论（各商店都不提供撤销）不同
	if caps.Withdrawal != channel.WithdrawalAPISupported {
		t.Errorf("Withdrawal = %v, 华为有撤销审核接口", caps.Withdrawal)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试")
	}
	if !strings.Contains(caps.VerifiedScope, "送审（app-submit）未验证") {
		t.Errorf("VerifiedScope 应写明送审未验证：%s", caps.VerifiedScope)
	}
}

// ---- 辅助 ----

// newSequencedHuawei 每被轮询一次就调用 next(次数) 取响应，便于测轮询行为。
func newSequencedHuawei(t *testing.T, next func(int) string) (*httptest.Server, *Channel) {
	t.Helper()
	fake := &fakeHuawei{responses: map[string]string{}}
	var mu sync.Mutex
	polls := 0

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.record(r, string(body))

		if strings.HasSuffix(r.URL.Path, "compile/status") {
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(next(n)))
			return
		}
		fake.mu.Lock()
		resp, ok := fake.responses[r.URL.Path]
		fake.mu.Unlock()
		if !ok {
			resp, ok = defaults[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ret":{"code":404,"msg":"unexpected"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	ch := NewWithBaseURL(srv.URL + "/")
	ch.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }
	ch.pollTimeouts = &pollConfig{interval: time.Millisecond, timeout: 2 * time.Second}
	fake.set("/api/publish/v2/upload-url/for-obs",
		`{"ret":{"code":0},"urlInfo":{"url":"`+srv.URL+uploadPath+`","objectId":"obj-1"}}`)
	fake.set(uploadPath, ``)
	return srv, ch
}
