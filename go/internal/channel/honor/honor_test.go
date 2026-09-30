package honor

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

// ---- 测试脚手架 ----

type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

type fakeHonor struct {
	mu        sync.Mutex
	responses map[string]string
	requests  []recorded
}

// set 设置某个路径的响应。
//
// 必须走锁：测试线程写、服务端 goroutine 读，裸赋值既是数据竞争，
// 也可能因为可见性问题丢失写入 —— 表现是「明明设了响应，请求却走了默认值」。
func (f *fakeHonor) set(path, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = resp
}

func (f *fakeHonor) record(r *http.Request, body string) {
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

func (f *fakeHonor) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeHonor) paths() []string {
	out := []string{}
	for _, r := range f.all() {
		out = append(out, r.Path)
	}
	return out
}

func (f *fakeHonor) find(suffix string) (recorded, bool) {
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			return r, true
		}
	}
	return recorded{}, false
}

func (f *fakeHonor) mustFind(t *testing.T, suffix string) recorded {
	t.Helper()
	r, ok := f.find(suffix)
	if !ok {
		t.Fatalf("没找到路径以 %q 结尾的请求，实际请求：%v", suffix, f.paths())
	}
	return r
}

// newFakeHonor 起一个记录请求的假网关，并返回渠道实例。
//
// 返回的渠道已经把 baseURL / tokenURL 都指向假网关 —— 生产环境里这两个是不同域名，
// 但测试关心的是「token 走的是独立地址」这件事由注入点覆盖。
func newFakeHonor(t *testing.T) (*Channel, *fakeHonor) {
	t.Helper()
	fake := &fakeHonor{responses: map[string]string{}}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.record(r, string(body))

		key := r.URL.Path
		fake.mu.Lock()
		resp, ok := fake.responses[key]
		fake.mu.Unlock()
		if !ok {
			resp, ok = defaults[key]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"unexpected path ` + key + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	ch := NewWithBaseURL(srv.URL+"/", srv.URL+"/auth/token")
	ch.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }

	// 上传地址要指向同一个假网关，否则会被 requireHTTPS 之外的原因拒掉
	fake.set("/openapi/v1/publish/get-file-upload-url",
		`{"code":0,"msg":"ok","data":[{"uploadUrl":"`+srv.URL+uploadPath+`","objectId":7788}]}`)
	fake.set(uploadPath, `{"code":0,"msg":"ok"}`)

	return ch, fake
}

// defaults 是各接口的默认成功响应。
//
// 前三个接口不给完整响应会让流程在第一步就断，测不到后面的步骤。
var defaults = map[string]string{
	"/auth/token": `{"access_token":"tok-honor"}`,
	"/openapi/v1/publish/get-app-id": `{"code":0,"msg":"ok","data":[` +
		`{"packageName":"com.example.app","appId":"app-123"}]}`,
	"/openapi/v1/publish/get-app-detail": `{"code":0,"msg":"ok","data":{` +
		`"languageInfo":[{"languageId":"zh-CN","appName":"示例应用","intro":"介绍","briefIntro":"简述"}],` +
		`"releaseInfo":{"versionCode":1000,"versionName":"1.0.0"},` +
		// basicInfo 是必须的：年龄分级未设置时送审会被拒（code=20046）
		`"basicInfo":{"ratingId":3,"appClassification":"11002",` +
		`"supplyName":"示例公司","supplyNameEn":"","devName":"示例公司","devNameEn":"",` +
		`"appCategoryId":2,"defaultLanguage":"zh-CN","releaseCountry":"CN",` +
		`"gameType":2,"paymentInfo":1,"privacyPolicyUrl":"https://p.example.com"}}}`,
	"/openapi/v1/publish/update-file-info":     `{"code":0,"msg":"ok"}`,
	"/openapi/v1/publish/update-language-info": `{"code":0,"msg":"ok"}`,
	"/openapi/v1/publish/submit-audit":         `{"code":0,"msg":"ok"}`,
	"/openapi/v1/publish/update-app-info":      `{"code":0,"msg":"ok"}`,
}

// uploadPath 是假网关上的上传地址路径。
const uploadPath = "/upload-received"

func testArtifactFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-HONOR-1.0.0.apk")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
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
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	reached, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	// 八步顺序必须完全一致。荣耀要求「先绑定文件再改语言信息」，
	// 顺序变动会导致提交的版本缺内容
	want := []string{
		"/auth/token",
		"/openapi/v1/publish/get-app-id",
		"/openapi/v1/publish/get-app-detail",
		"/openapi/v1/publish/get-file-upload-url",
		uploadPath,
		"/openapi/v1/publish/update-file-info",
		"/openapi/v1/publish/update-language-info",
		// 送审前再查一次应用详情：确认年龄分级已设置（未设置会被送审拒绝）
		"/openapi/v1/publish/get-app-detail",
		"/openapi/v1/publish/submit-audit",
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

func TestTokenUsesFormEncoding(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	tokenReq := fake.all()[0]
	if tokenReq.Method != http.MethodPost {
		t.Errorf("token 请求方法 = %s, 期望 POST", tokenReq.Method)
	}
	// 荣耀 IAM 只接受表单，改成 JSON 会直接失败
	if ct := tokenReq.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("token 的 Content-Type = %q, 荣耀 IAM 只接受表单编码", ct)
	}
	form, err := url.ParseQuery(tokenReq.Body)
	if err != nil {
		t.Fatalf("token 请求体不是表单编码：%q", tokenReq.Body)
	}
	if form.Get("client_id") != "test-client-id" ||
		form.Get("client_secret") != "test-client-secret" ||
		form.Get("grant_type") != "client_credentials" {
		t.Errorf("token 表单内容不对：%v", form)
	}
}

func TestBearerTokenOnBusinessRequests(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	for i, r := range fake.all() {
		if r.Path == "/auth/token" {
			continue // token 请求自己不带 Authorization
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-honor" {
			t.Errorf("第 %d 个请求（%s）的 Authorization = %q, 期望 Bearer tok-honor",
				i+1, r.Path, got)
		}
	}
	// appId 走 query，但只有用到 appId 的接口才带：
	// get-app-id 用的是 pkgName，上传地址是服务端下发的（自带参数）
	byAppID := map[string]bool{
		"/openapi/v1/publish/get-app-detail":       true,
		"/openapi/v1/publish/get-file-upload-url":  true,
		"/openapi/v1/publish/update-file-info":     true,
		"/openapi/v1/publish/update-language-info": true,
		"/openapi/v1/publish/submit-audit":         true,
	}
	for _, r := range fake.all() {
		if !byAppID[r.Path] {
			continue
		}
		if got := r.Query.Get("appId"); got != "app-123" {
			t.Errorf("%s 的 appId = %q, 期望 app-123", r.Path, got)
		}
	}
	// get-app-id 必须用 pkgName
	if r, ok := fake.find("get-app-id"); ok {
		if got := r.Query.Get("pkgName"); got != "com.example.app" {
			t.Errorf("get-app-id 的 pkgName = %q", got)
		}
	}
}

func TestUploadURLRequestCarriesSHA256AndFileType(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	req := fake.mustFind(t, "get-file-upload-url")
	var payload []uploadFile
	if err := json.Unmarshal([]byte(req.Body), &payload); err != nil {
		t.Fatalf("请求体不是文件描述数组：%q", req.Body)
	}
	if len(payload) != 1 {
		t.Fatalf("文件描述个数 = %d, 期望 1", len(payload))
	}
	f := payload[0]
	// 100 是荣耀约定的 APK 类型，不要改成其他数字
	if f.FileType != APKFileType {
		t.Errorf("fileType = %d, 期望 %d（荣耀约定的 APK 类型）", f.FileType, APKFileType)
	}
	if f.FileSize != 4096 {
		t.Errorf("fileSize = %d, 期望 4096", f.FileSize)
	}
	if len(f.FileSha256) != 64 {
		t.Errorf("fileSha256 长度 = %d, 期望 64（SHA-256 的十六进制）", len(f.FileSha256))
	}
	if f.FileName != "app-HONOR-1.0.0.apk" {
		t.Errorf("fileName = %q", f.FileName)
	}
}

func TestBindFileSendsObjectID(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	req := fake.mustFind(t, "update-file-info")
	if !strings.Contains(req.Body, `"bindingFileList"`) || !strings.Contains(req.Body, "7788") {
		t.Errorf("绑定请求应回传 objectId：%s", req.Body)
	}
}

func TestUpdateLanguageInfoReusesOnlineFields(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	req := fake.mustFind(t, "update-language-info")
	var payload versionDesc
	if err := json.Unmarshal([]byte(req.Body), &payload); err != nil {
		t.Fatalf("请求体解析失败：%q", req.Body)
	}
	if len(payload.List) != 1 {
		t.Fatalf("languageInfoList 长度 = %d", len(payload.List))
	}
	item := payload.List[0]
	// appName / intro 必填，所以用线上语言信息原样回填
	if item.AppName != "示例应用" || item.Intro != "介绍" {
		t.Errorf("应当回填线上语言信息，实际 appName=%q intro=%q", item.AppName, item.Intro)
	}
	if item.NewFeature != "修复若干问题" {
		t.Errorf("newFeature = %q, 应当是本次的更新说明", item.NewFeature)
	}
	if item.LanguageID != "zh-CN" {
		t.Errorf("languageId = %q", item.LanguageID)
	}
}

func TestScheduledReleaseTimeFormat(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	req := testRequest(t, path, channel.StageSubmitReview)
	// 2026-01-15 10:00:00 本地时间
	req.ReleaseParams.OnlineTime = time.Date(2026, 1, 15, 10, 0, 0, 0, time.Local).UnixMilli()

	if _, err := ch.Upload(context.Background(), req); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "submit-audit")
	var param submitParam
	if err := json.Unmarshal([]byte(submit.Body), &param); err != nil {
		t.Fatalf("请求体解析失败：%q", submit.Body)
	}
	if param.ReleaseType != 2 {
		t.Errorf("releaseType = %d, 定时发布应为 2", param.ReleaseType)
	}
	if param.ReleaseTime == nil {
		t.Fatal("定时发布必须带 releaseTime")
	}
	// 关键：荣耀要 +0800 而不是 +08:00。
	// Go 的 "Z07:00" 布局会产出带冒号的形式，必须用 "-0700"
	got := *param.ReleaseTime
	if !strings.HasPrefix(got, "2026-01-15T10:00:00") {
		t.Errorf("releaseTime = %q", got)
	}
	if strings.Contains(got, ":00:00+08:00") || strings.HasSuffix(got, "+08:00") {
		t.Errorf("releaseTime = %q 用了带冒号的时区偏移，荣耀要求 +0800 形式", got)
	}
	if !strings.HasSuffix(got, "+0800") {
		t.Errorf("releaseTime = %q, 期望以 +0800 结尾", got)
	}
}

func TestImmediateReleaseHasNoReleaseTime(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "submit-audit")
	var param submitParam
	_ = json.Unmarshal([]byte(submit.Body), &param)
	if param.ReleaseType != 1 {
		t.Errorf("releaseType = %d, 立即发布应为 1", param.ReleaseType)
	}
	if param.ReleaseTime != nil {
		t.Errorf("立即发布不应带 releaseTime，实际 %q", *param.ReleaseTime)
	}
}

// ---- 停留点 ----

func TestStopAtArtifactSkipsBindAndSubmit(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	reached, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageUploadArtifact))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageUploadArtifact {
		t.Errorf("reached = %v", reached)
	}

	for _, p := range fake.paths() {
		switch {
		case strings.HasSuffix(p, "update-file-info"):
			t.Error("停在仅上传时不应绑定文件")
		case strings.HasSuffix(p, "update-language-info"):
			t.Error("停在仅上传时不应改更新说明")
		case strings.HasSuffix(p, "submit-audit"):
			t.Error("停在仅上传时不应送审")
		}
	}
}

func TestStopAtDraftSkipsSubmit(t *testing.T) {
	ch, fake := newFakeHonor(t)

	path := testArtifactFile(t)
	reached, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageCreateDraft))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageCreateDraft {
		t.Errorf("reached = %v", reached)
	}

	// 草稿态要求「文件已绑定 + 更新描述已写入」，这两步必须做过
	var bound, described bool
	for _, p := range fake.paths() {
		if strings.HasSuffix(p, "update-file-info") {
			bound = true
		}
		if strings.HasSuffix(p, "update-language-info") {
			described = true
		}
		if strings.HasSuffix(p, "submit-audit") {
			t.Error("停在草稿态时不应送审")
		}
	}
	if !bound || !described {
		t.Errorf("草稿态需要文件已绑定且描述已写入，实际 bound=%v described=%v", bound, described)
	}
}

func TestUploadRequiresTokenAndAppID(t *testing.T) {
	// 每一步失败都要带上动作名，否则用户只看到一句英文异常
	cases := []struct {
		name      string
		responses map[string]string
		wantMsg   string
	}{
		{
			name:      "token 为空",
			responses: map[string]string{"/auth/token": `{"access_token":""}`},
			wantMsg:   "access_token",
		},
		{
			name: "找不到 appId",
			responses: map[string]string{
				"/openapi/v1/publish/get-app-id": `{"code":0,"msg":"ok","data":[]}`,
			},
			wantMsg: "找不到包名",
		},
		{
			name: "语言信息为空",
			responses: map[string]string{
				"/openapi/v1/publish/get-app-detail": `{"code":0,"msg":"ok","data":{"languageInfo":[]}}`,
			},
			wantMsg: "语言信息",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, fake := newFakeHonor(t)
			for k, v := range tc.responses {
				fake.set(k, v)
			}

			path := testArtifactFile(t)
			_, err := ch.Upload(context.Background(),
				testRequest(t, path, channel.StageSubmitReview))
			if err == nil {
				t.Fatal("期望失败")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("错误信息应含 %q，实际：%v", tc.wantMsg, err)
			}
		})
	}
}

func TestChannelRejectedSurfacesCode(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/submit-audit", `{"code":204144660,"msg":"[cds]submit failed"}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	if !strings.Contains(err.Error(), "204144660") {
		t.Errorf("应带上渠道返回的错误码：%v", err)
	}
	if !strings.Contains(err.Error(), "submit failed") {
		t.Errorf("应带上渠道返回的描述：%v", err)
	}
}

func TestUploadAddressMustBeHTTPS(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-file-upload-url",
		`{"code":0,"msg":"ok","data":[{"uploadUrl":"http://insecure.example.com/up","objectId":1}]}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("http 上传地址应当被拒绝")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("应说明拒绝原因：%v", err)
	}
}

func TestSubmitFailureIsMarkedAtSubmissionPoint(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/submit-audit", `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	pe, ok := err.(interface {
		Retryable() bool
	})
	if !ok {
		t.Fatalf("错误类型不支持 Retryable(): %T", err)
	}
	if pe.Retryable() {
		t.Error("送审点的失败绝不能标记为可重试")
	}
	if !strings.Contains(err.Error(), "确认") {
		t.Errorf("提示应引导先到后台确认：%v", err)
	}
}

func TestPreSubmitFailureStaysRetryable(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-app-id", `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(),
		testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	if strings.Contains(err.Error(), "确认该版本是否已提交成功") {
		t.Errorf("送审前的失败不应带送审点提示：%v", err)
	}
}

// ---- 状态查询 ----

func TestQueryMarketMapsAuditResult(t *testing.T) {
	cases := []struct {
		auditResult int
		want        channel.ReviewState
	}{
		{0, channel.ReviewUnderReview},
		{1, channel.ReviewOnline},
		{2, channel.ReviewRejected},
		// 3 是「其他非审核状态」，官方未细分。归 Offline 而不是 Unknown：
		// 未知会让 checklist 标成「要求人工核实」，而 3 已经确定不是审核中、
		// 不是已上架、也不是被拒 —— 提交新版本不会因此被挡。
		// 精确状态（下架/撤销/冻结）由 RawStateLabel 透传官方原文。
		{3, channel.ReviewOffline},
		{4, channel.ReviewDraft},
		{99, channel.ReviewUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.auditResult), func(t *testing.T) {
			ch, fake := newFakeHonor(t)
			fake.set("/openapi/v1/publish/get-app-current-release", fmt.Sprintf(
				`{"code":0,"msg":"ok","data":{"auditResult":%d,"versionCode":1000,"versionName":"1.0.0"}}`,
				tc.auditResult))

			info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
				ApplicationID: "com.example.app",
				Credentials: channel.NewCredentials(map[string]string{
					ParamClientID: "k", ParamClientSecret: "s",
				}),
				Timeouts: httpx.Default(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.ReviewState != tc.want {
				t.Errorf("auditResult=%d 映射为 %v, 期望 %v", tc.auditResult, info.ReviewState, tc.want)
			}
			// 原始值留给排查：映射到 Unknown 时至少能看到渠道给了什么
			if !strings.Contains(info.RawState, fmt.Sprint(tc.auditResult)) {
				t.Errorf("RawState = %q, 应含原始取值", info.RawState)
			}
		})
	}
}

func TestQueryMarketWithoutVersionDoesNotFabricate(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-app-current-release", `{"code":0,"msg":"ok","data":{"auditResult":4}}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 商店里只有草稿版本时荣耀不返回版本号。伪造 0 会让版本号比对得出错误结论
	if info.LastVersion != nil {
		t.Errorf("LastVersion = %v, 期望 nil", info.LastVersion)
	}
	if info.ReviewState != channel.ReviewDraft {
		t.Errorf("ReviewState = %v, auditResult=4 是编辑中未提审", info.ReviewState)
	}
}

// ---- 声明 ----

// checkSuccess 必须把渠道错误码写进给人看的 message。
//
// 用户拿这个码去查渠道文档；只给一句自然语言描述等于把唯一的线索藏起来。
// 上游的 ApiException 只拼 message 不带 code，排查时得靠翻日志找原始响应。
func TestRejectedErrorMentionsCodeInMessage(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/submit-audit", `{"code":204144660,"msg":"submit failed"}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Code != "204144660" {
		t.Errorf("Code 字段 = %q", pe.Code)
	}
	if !strings.Contains(pe.Msg, "code=204144660") {
		t.Errorf("message 里应带上错误码，实际：%s", pe.Msg)
	}
}

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
			t.Errorf("SupportedStages[%d] = %v, 期望 %v", i, caps.SupportedStages[i], want[i])
		}
	}
	if caps.RiskLevel != channel.RiskHigh {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试")
	}
	if !caps.RequiresExplicitConfirmation {
		t.Error("送审必须要求显式确认")
	}
	if !strings.Contains(caps.VerifiedScope, "送审未验证") {
		t.Errorf("VerifiedScope 应写明送审未验证：%s", caps.VerifiedScope)
	}
}

func TestParamsDeclaration(t *testing.T) {
	params := New().Params()
	if len(params) != 2 {
		t.Fatalf("参数个数 = %d", len(params))
	}
	if params[0].Name != ParamClientID || params[1].Name != ParamClientSecret {
		t.Errorf("参数名不对：%v %v", params[0].Name, params[1].Name)
	}
	for _, p := range params {
		if !p.Required || p.Description == "" {
			t.Errorf("%s 应为必填且有说明", p.Name)
		}
	}
}

// TestRealResponseShapesAreParsed 覆盖荣耀响应里「类型与文档不符」的字段。
//
// 回归背景：Go 的 encoding/json 是严格类型匹配，而荣耀的 appId 实际返回裸数字
// （文档写作字符串）。原来的测试 fixture 用的是 "app-123" 这种理想形态，
// 所以解析缺陷在单元测试里测不出来 —— 只有真实调用才暴露。
// 这个测试用线上响应的真实形状，把当时的失败固化成回归保护。
func TestRealResponseShapesAreParsed(t *testing.T) {
	ch, fake := newFakeHonor(t)
	// appId 是数字，不是字符串
	fake.set("/openapi/v1/publish/get-app-id",
		`{"code":0,"msg":"Success","data":[{"packageName":"com.example.app","appId":900876322}]}`)
	// versionCode 也可能以字符串返回
	fake.set("/openapi/v1/publish/get-app-current-release",
		`{"code":0,"msg":"ok","data":{"auditResult":1,"versionCode":"1000","versionName":"1.0.0"}}`)
	fake.set("/openapi/v1/publish/get-app-detail",
		`{"code":0,"msg":"ok","data":{"languageInfo":[{"languageId":"zh-CN","languageName":"简体中文"}]}}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials: channel.NewCredentials(map[string]string{
			ParamClientID: "k", ParamClientSecret: "s",
		}),
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatalf("真实形状的响应应能解析，却失败: %v", err)
	}
	if info.ReviewState != channel.ReviewOnline {
		t.Errorf("状态 = %v, 期望已上架", info.ReviewState)
	}
	if info.LastVersion == nil {
		t.Fatal("versionCode 为字符串数字时应解析出版本信息")
	}
	if info.LastVersion.Code != 1000 {
		t.Errorf("versionCode = %d, 期望 1000", info.LastVersion.Code)
	}
	// appId 是数字，须原样转成字符串 "900876322" 用于后续请求
	if r, ok := fake.find("get-app-current-release"); ok {
		if got := r.Query.Get("appId"); got != "900876322" {
			t.Errorf("后续请求携带的 appId = %q, 期望 \"900876322\"（数字须转为十进制字符串）", got)
		}
	} else {
		t.Error("未发出 get-app-current-release 请求")
	}
}

// TestEnsureRating 覆盖年龄分级（RatingId）的自动补齐。
//
// 回归背景：荣耀的年龄分级是送审的必填前置，未设置时 submit-audit 返回
// `app rating id is empty (code=20046)` —— 提示里既不说去哪设置、也不说是哪个字段。
// 实测只能靠比对 get-app-detail 的 basicInfo.ratingId 是否为 null 定位。
// 线上 civilian 就卡在这个错误上，而 gwy 因为早期在后台设过所以正常。
func TestEnsureRating(t *testing.T) {
	t.Run("未设置时补齐并调用 update-app-info", func(t *testing.T) {
		ch, fake := newFakeHonor(t)
		fake.set("/openapi/v1/publish/get-app-detail",
			`{"code":0,"msg":"ok","data":{"languageInfo":[{"languageId":"zh-CN","appName":"x"}],`+
				`"basicInfo":{"ratingId":null,"appClassification":"11002","supplyName":"公司",`+
				`"defaultLanguage":"zh-CN","releaseCountry":"CN","paymentInfo":1}}}`)

		fixed, err := testAPI(ch).EnsureRating(context.Background(), "tok", "app-1")
		if err != nil {
			t.Fatal(err)
		}
		if !fixed {
			t.Error("ratingId 为 null 时应上报「已补齐」")
		}
		if _, ok := fake.find("update-app-info"); !ok {
			t.Error("未调用 update-app-info，年龄分级不会被设置")
		}
	})

	t.Run("已设置时不写入", func(t *testing.T) {
		ch, fake := newFakeHonor(t)
		// 默认 fixture 的 basicInfo.ratingId = 3
		fixed, err := testAPI(ch).EnsureRating(context.Background(), "tok", "app-1")
		if err != nil {
			t.Fatal(err)
		}
		if fixed {
			t.Error("已设置时不应写入 —— 会覆盖运营在后台选定的等级")
		}
		if _, ok := fake.find("update-app-info"); ok {
			t.Error("已设置 ratingId 时不应调用 update-app-info")
		}
	})

	t.Run("缺 basicInfo 时报错而非静默", func(t *testing.T) {
		ch, fake := newFakeHonor(t)
		fake.set("/openapi/v1/publish/get-app-detail",
			`{"code":0,"msg":"ok","data":{"languageInfo":[]}}`)
		if _, err := testAPI(ch).EnsureRating(context.Background(), "tok", "app-1"); err == nil {
			t.Fatal("缺 basicInfo 时应报错：此时写入会误清空应用资料")
		}
	})
}

// testAPI 用假网关构造 API 实例，供不经过 Channel.Upload 的单元测试使用。
func testAPI(ch *Channel) *API {
	return NewAPI(ch.client(httpx.Default()), ch.baseURL, ch.tokenURL)
}

// ---- 审核意见 ----

// TestQueryMarketCarriesAuditMessage 验证荣耀的审核意见与附件被带出来。
//
// 字段名与取值来自官方《API传包服务指引》的 PubAuditResult：
//
//	auditMessage    否  String        审核意见
//	auditAttachment 否  List<String>  审核意见附件，为url，可查看或下载
//
// 附件特别值得留意：审核员截的图往往比文字说明更具体。
func TestQueryMarketCarriesAuditMessage(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-app-current-release", `{
		"code": 0, "msg": "ok",
		"data": {
			"releaseId": "123456789",
			"auditResult": 2,
			"auditMessage": "应用内存在未声明的权限申请，请补充隐私政策说明",
			"auditAttachment": ["https://xxx.com/image/a.webp", "https://xxx.com/image/b.webp"],
			"versionName": "1.0.5",
			"versionCode": "105"
		}
	}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewRejected {
		t.Errorf("auditResult=2 应映射为审核被拒，实际 %v", info.ReviewState)
	}
	if info.Review == nil {
		t.Fatal("应带上审核意见")
	}
	if !strings.Contains(info.Review.Opinion, "未声明的权限") {
		t.Errorf("审核意见内容不对：%q", info.Review.Opinion)
	}
	if len(info.Review.Attachments) != 2 {
		t.Errorf("附件应有 2 个，实际 %d 个：%v",
			len(info.Review.Attachments), info.Review.Attachments)
	}
	// 附件是 URL，不该被改写
	if info.Review.Attachments[0] != "https://xxx.com/image/a.webp" {
		t.Errorf("附件 URL 被改动了：%q", info.Review.Attachments[0])
	}
}

// TestQueryMarketAuditMessageEmptyLeavesNoShell 覆盖「字段在但没内容」。
//
// auditMessage 在文档里标为「否」（非必填），未拒审时通常没有值。
func TestQueryMarketAuditMessageEmptyLeavesNoShell(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-app-current-release", `{
		"code": 0, "msg": "ok",
		"data": {"auditResult": 1, "auditMessage": "", "versionCode": 1000, "versionName": "1.0.0"}
	}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Review != nil {
		t.Errorf("意见为空时不应留下空壳：%+v", info.Review)
	}
}

// TestQueryMarketAuditMessageOnApproval 记录一个反直觉的实际情况。
//
// 荣耀的官方调用示例里，审核**通过**（auditResult=1）时 auditMessage 也有内容：
//
//	"auditMessage": "审核通过：XXX"
//
// 所以不能假设「有意见 = 被拒」，两个字段要分开看。
func TestQueryMarketAuditMessageOnApproval(t *testing.T) {
	ch, fake := newFakeHonor(t)
	fake.set("/openapi/v1/publish/get-app-current-release", `{
		"code": 0, "msg": "ok",
		"data": {"auditResult": 1, "auditMessage": "审核通过：XXX", "versionCode": 1000, "versionName": "1.0.0"}
	}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewOnline {
		t.Errorf("auditResult=1 应映射为已上架，实际 %v", info.ReviewState)
	}
	// 有意见不等于被拒 —— 状态与意见是独立的两个字段
	if info.Review == nil {
		t.Error("审核通过时也可能带意见，不应丢弃")
	}
}
