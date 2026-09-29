package oppo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

const goldenPath = "../../../testdata/golden/signing.json"

type oppoGolden struct {
	Name      string             `json:"name"`
	Secret    string             `json:"secret"`
	Params    map[string]*string `json:"params"`
	Canonical string             `json:"canonical"`
	Signature string             `json:"signature"`
}

// TestSignMatchesGoldenVectors 是 OPPO 签名的核心验收。
//
// fixture 由 Kotlin 侧生成，并已用 Python 的 hmac/hashlib 独立复算过，
// 因此它不是「Kotlin 说自己对」，而是两个独立实现都同意。
func TestSignMatchesGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读取黄金向量失败（%v）。先在仓库根目录运行：\n"+
			"  ./gradlew :core:test --tests \"*GoldenVectorTest*\" -Dgolden.update=true --rerun-tasks", err)
	}
	var golden struct {
		Oppo []oppoGolden `json:"oppo"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Oppo) == 0 {
		t.Fatal("黄金向量里没有 oppo 用例")
	}

	for _, v := range golden.Oppo {
		t.Run(v.Name, func(t *testing.T) {
			if got := Canonicalize(v.Params); got != v.Canonical {
				t.Errorf("待签串不一致\n  Go:     %s\n  golden: %s", got, v.Canonical)
			}
			if got := Sign(v.Secret, v.Params); got != v.Signature {
				t.Errorf("签名不一致\n  Go:     %s\n  golden: %s", got, v.Signature)
			}
		})
	}
}

func TestCanonicalizeRuleNilValueSkipped(t *testing.T) {
	// 值为 nil 的参数整体跳过，连键名也不参与。
	// 这不是防御性代码：调用方在算 api_sign 之前会把它自己以 nil 值放进参数表，
	// 跳过与否直接决定签名能不能通过
	got := Canonicalize(map[string]*string{
		"a":        ptr("1"),
		"api_sign": nil,
		"c":        ptr("3"),
	})
	if got != "a=1&c=3" {
		t.Errorf("got %q, want %q", got, "a=1&c=3")
	}
}

func TestCanonicalizeEmptyStringStillParticipates(t *testing.T) {
	// 空串与「键不存在」不同：空串仍然产出 `key=`
	got := Canonicalize(map[string]*string{"a": ptr(""), "b": ptr("2")})
	if got != "a=&b=2" {
		t.Errorf("got %q, want %q", got, "a=&b=2")
	}
}

func TestCanonicalizeSortsByKey(t *testing.T) {
	got := Canonicalize(map[string]*string{
		"z": ptr("1"), "a": ptr("2"), "m": ptr("3"),
	})
	if got != "a=2&m=3&z=1" {
		t.Errorf("got %q", got)
	}
}

func ptr(s string) *string { return &s }

// ---- 全流程 ----

// fakeOppo 记录请求并按路径返回响应。
type fakeOppo struct {
	requests []*url.URL
	bodies   []string
}

func TestUploadFullFlowRequestShapes(t *testing.T) {
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken: `{"errno":0,"data":{"access_token":"tok-abc"}}`,
		pathAppInfo: `{"errno":0,"data":{"summary":"一句话","detail_desc":"详细",` +
			`"version_code":"1000","version_name":"1.0.0","audit_status":111,` +
			`"privacy_source_url":"https://p.example.com","ver_second_category_id":"2",` +
			`"ver_third_category_id":"30","icon_url":"https://i.png","pic_url":"https://p.png",` +
			`"test_desc":"","business_username":"","business_email":"","business_mobile":"",` +
			`"copyright_url":"https://c.pdf","electronic_cert_url":"https://e.pdf"}}`,
		pathUploadURL: `{"errno":0,"data":{"upload_url":"` + "" + `","sign":"once-sign"}}`,
		pathSubmit:    `{"errno":0}`,
	})
	// upload_url 必须指向假服务器，否则会被 requireHTTPS 拒绝（协议不符）
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload",` +
		`"sign":"once-sign"}}`
	fake.responses["/upload"] = `{"errno":0,"data":{"url":"https://cdn.example.com/app.apk","md5":"abc123"}}`

	path := testArtifactFile(t)
	reached, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	paths := fake.paths()
	want := []string{pathToken, pathAppInfo, pathUploadURL, "/upload", pathSubmit}
	if len(paths) != len(want) {
		t.Fatalf("请求次数 = %d, 期望 %d：%v", len(paths), len(want), paths)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("第 %d 个请求路径 = %q, 期望 %q（顺序：token → app/info → upload-url → 上传 → 提交）",
				i+1, paths[i], w)
		}
	}

	// 取 token 是 GET + query，且 client_id/client_secret 在 query 上
	tokenURL := fake.urls[0]
	if tokenURL.Query().Get("client_id") != "test-client-id" {
		t.Errorf("token 请求的 client_id = %q", tokenURL.Query().Get("client_id"))
	}
	if tokenURL.Query().Get("client_secret") != "test-client-secret" {
		t.Errorf("token 请求的 client_secret = %q", tokenURL.Query().Get("client_secret"))
	}

	// 后续每个请求都要带 access_token / timestamp / api_sign 三元组
	for i, u := range fake.urls[1:] {
		for _, key := range []string{"access_token", "timestamp", "api_sign"} {
			if u.Query().Get(key) == "" {
				t.Errorf("第 %d 个请求缺少签名参数 %s", i+2, key)
			}
		}
		if u.Query().Get("access_token") != "tok-abc" {
			t.Errorf("access_token = %q", u.Query().Get("access_token"))
		}
	}

	// timestamp 是秒级（10 位），与 vivo 的毫秒不同
	ts := fake.urls[1].Query().Get("timestamp")
	if len(ts) != 10 {
		t.Errorf("timestamp = %q（长度 %d），OPPO 用秒级时间戳", ts, len(ts))
	}
}

func TestSubmitSendsAllRequiredStoreFields(t *testing.T) {
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken:     `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo:   appInfoResponse(),
		pathUploadURL: `{"errno":0,"data":{"upload_url":"PLACEHOLDER/upload","sign":"s"}}`,
		"/upload":     `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
		pathSubmit:    `{"errno":0}`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	if _, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	// app/upd 是全量更新语义：漏任何一个字段就会被清空或被拒
	submit := fake.bodies[len(fake.bodies)-1]
	form, err := url.ParseQuery(submit)
	if err != nil {
		t.Fatalf("提交体不是 form 编码: %v", err)
	}
	expect := map[string]string{
		"pkg_name":            "com.example.app",
		"version_code":        "1020",
		"update_desc":         "修复若干问题",
		"online_type":         "1",
		"summary":             "一句话",
		"detail_desc":         "详细",
		"privacy_source_url":  "https://p.example.com",
		"second_category_id":  "2",
		"third_category_id":   "30",
		"icon_url":            "https://i.png",
		"pic_url":             "https://p.png",
		"copyright_url":       "https://c.pdf",
		"electronic_cert_url": "https://e.pdf",
	}
	for key, want := range expect {
		if got := form.Get(key); got != want {
			t.Errorf("提交参数 %s = %q, 期望 %q", key, got, want)
		}
	}
	// apk_url 是 JSON 数组字符串，固定三个字段
	apkURL := form.Get("apk_url")
	if !strings.Contains(apkURL, `"url":"https://cdn/app.apk"`) ||
		!strings.Contains(apkURL, `"md5":"m"`) ||
		!strings.Contains(apkURL, `"cpu_code":0`) {
		t.Errorf("apk_url 结构不对：%s", apkURL)
	}
}

func TestCopyrightFallsBackToElectronicCert(t *testing.T) {
	// OPPO 要求 copyright_url 非空，而多数开发者只上传了电子版软著
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: strings.Replace(appInfoResponse(), `"copyright_url":"https://c.pdf"`, `"copyright_url":""`, 1),
		"/upload":   `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
		pathSubmit:  `{"errno":0}`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	if _, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	form, _ := url.ParseQuery(fake.bodies[len(fake.bodies)-1])
	if got := form.Get("copyright_url"); got != "https://e.pdf" {
		t.Errorf("copyright_url = %q, 应当回退到电子版软著", got)
	}
}

func TestMissingStoreFieldGivesActionableError(t *testing.T) {
	// 商店资料缺失属于 precondition，提示要指向可执行的动作 ——
	// 与「接口变更」区分开，后者是 protocol 错误
	srv, _ := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: strings.Replace(appInfoResponse(), `"icon_url":"https://i.png"`, `"icon_url":""`, 1),
	})
	path := testArtifactFile(t)

	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindPrecondition {
		t.Fatalf("期望 Precondition，实际 %v", err)
	}
	if !strings.Contains(pe.Msg, "icon_url") {
		t.Errorf("应指出缺的是哪个字段：%s", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "OPPO 开放平台补全") {
		t.Errorf("应给出可执行的动作：%s", pe.Msg)
	}
}

func TestErrnoCheckedBeforeDataParsing(t *testing.T) {
	// 失败响应里 data 的形状与成功时不同（可能是数组或字符串）。
	// 若先按成功结构解析，抛出的会是解析异常，真正的业务错误码与 message 都被吞掉
	srv, _ := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: `{"errno":800002,"data":["icon_url 不允许的文件格式"]}`,
	})
	path := testArtifactFile(t)

	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Kind != eperr.KindChannelRejected {
		t.Errorf("Kind = %v, 应当是渠道拒绝而不是解析失败", pe.Kind)
	}
	if pe.Code != "800002" {
		t.Errorf("Code = %q", pe.Code)
	}
	if !strings.Contains(pe.Raw, "icon_url 不允许的文件格式") {
		t.Errorf("原始响应应保留下来便于排查：%s", pe.Raw)
	}
}

func TestMissingErrnoIsProtocolErrorNotSuccess(t *testing.T) {
	// 上游是 get("errno").asInt —— 字段缺失时 NPE，把本该抛出的业务错误顶掉。
	// 这里按协议不符处理：不能静默当成成功
	srv, _ := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: `{"data":{"summary":"x"}}`,
	})
	path := testArtifactFile(t)

	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("缺少 errno 时应按失败处理")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
	if !strings.Contains(pe.Msg, "errno") {
		t.Errorf("应说明缺的是 errno：%s", pe.Msg)
	}
}

func TestTokenFailureDoesNotLeakTokenInRaw(t *testing.T) {
	// token 接口的响应体里含裸 access_token。出错时不能把它放进错误的 Raw 字段
	srv, _ := newFakeOppo(t, map[string]string{
		pathToken: `{"errno":11001,"data":{"message":"client_secret 无效","access_token":"SHOULD-NOT-LEAK"}}`,
	})
	path := testArtifactFile(t)

	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if strings.Contains(pe.Raw, "SHOULD-NOT-LEAK") {
		t.Errorf("token 响应体不得进入错误的 Raw 字段：%s", pe.Raw)
	}
	if !strings.Contains(pe.Msg, "client_secret 无效") {
		t.Errorf("错误描述仍要保留：%s", pe.Msg)
	}
}

func TestSubmitFailureIsMarkedAtSubmissionPoint(t *testing.T) {
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: appInfoResponse(),
		"/upload":   `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
		pathSubmit:  `<html>502 Bad Gateway</html>`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v, app/upd 是送审动作，其失败必须标记", pe.Phase)
	}
	if pe.Retryable() {
		t.Error("送审点的失败绝不能标记为可重试")
	}
	if !strings.Contains(pe.Msg, "确认") {
		t.Errorf("提示应引导先到后台确认：%s", pe.Msg)
	}
}

func TestUploadURLMustBeHTTPS(t *testing.T) {
	// 上传地址来自接口响应，可能被篡改为 http ——
	// 明文发出去的是整个安装包与 access_token
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: appInfoResponse(),
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"http://insecure.example.com/upload","sign":"s"}}`

	path := testArtifactFile(t)
	_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path))
	if err == nil {
		t.Fatal("http 上传地址应当被拒绝")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
	if !strings.Contains(pe.Msg, "https") {
		t.Errorf("应说明拒绝原因：%s", pe.Msg)
	}
}

func TestStopAtArtifactDoesNotSubmit(t *testing.T) {
	srv, fake := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: appInfoResponse(),
		"/upload":   `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	req := testRequest(t, path)
	req.StopAfter = channel.StageUploadArtifact

	reached, err := newTestChannel(srv).Upload(context.Background(), req)
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageUploadArtifact {
		t.Errorf("reached = %v", reached)
	}
	for _, p := range fake.paths() {
		if p == pathSubmit {
			t.Error("停在仅上传时不应调用 app/upd")
		}
	}
}

func TestRejectsUnsupportedStage(t *testing.T) {
	// OPPO 没有草稿态。必须显式报错，不能默默走到送审
	_, err := New().Upload(context.Background(), channel.UploadRequest{
		StopAfter: channel.StageCreateDraft,
	})
	if err == nil {
		t.Fatal("期望报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration，实际 %v", err)
	}
}

func TestQueryMarketMapsAuditStatus(t *testing.T) {
	cases := []struct {
		status int
		want   channel.ReviewState
	}{
		{auditOnline, channel.ReviewOnline},
		{auditRejected, channel.ReviewRejected},
		{1, channel.ReviewUnderReview},
		{99, channel.ReviewUnderReview},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv, _ := newFakeOppo(t, map[string]string{
				pathToken: `{"errno":0,"data":{"access_token":"tok"}}`,
				pathAppInfo: strings.Replace(appInfoResponse(),
					`"audit_status":111`, fmt.Sprintf(`"audit_status":%d`, tc.status), 1),
			})
			info, err := newTestChannel(srv).QueryMarket(context.Background(), channel.MarketQuery{
				ApplicationID: "com.example.app",
				Credentials: channel.NewCredentials(map[string]string{
					ParamClientID: "test-client-id", ParamClientSecret: "test-client-secret",
				}),
				Timeouts: httpx.Default(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.ReviewState != tc.want {
				t.Errorf("audit_status=%d 映射为 %v, 期望 %v", tc.status, info.ReviewState, tc.want)
			}
		})
	}
}

func TestAuditStatusMissingIsUnknownNotUnderReview(t *testing.T) {
	// 接口没返回状态和商店确实在审核是两件事，混在一起会让排查走错方向。
	// 上游在这里会 NPE
	srv, _ := newFakeOppo(t, map[string]string{
		pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo: strings.Replace(appInfoResponse(), `"audit_status":111,`, "", 1),
	})
	info, err := newTestChannel(srv).QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamClientID: "k", ParamClientSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewUnknown {
		t.Errorf("ReviewState = %v, 期望 Unknown", info.ReviewState)
	}
	if info.RawState != "" {
		t.Errorf("RawState = %q, 未返回状态时应为空", info.RawState)
	}
}

func TestCapabilitiesDeclaration(t *testing.T) {
	caps := New().Capabilities()
	if len(caps.SupportedStages) != 2 ||
		caps.SupportedStages[0] != channel.StageUploadArtifact ||
		caps.SupportedStages[1] != channel.StageSubmitReview {
		t.Errorf("SupportedStages = %v", caps.SupportedStages)
	}
	if caps.RiskLevel != channel.RiskCritical {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试")
	}
	if !strings.Contains(caps.VerifiedScope, "送审（app/upd）未验证") {
		t.Errorf("VerifiedScope 应写明送审未验证：%s", caps.VerifiedScope)
	}
	if caps.Supports(channel.StageCreateDraft) {
		t.Error("OPPO 没有草稿态")
	}
}

// ---- 测试脚手架 ----

// newTestChannel 构造指向假网关的渠道，并注入信任其自签证书的客户端。
//
// httptest.NewTLSServer 用的是自签证书，httpx.Client 的默认 transport 不信任它；
// 而 OPPO 的 requireHTTPS 又拒绝明文地址，所以两者都得满足。
func newTestChannel(srv *httptest.Server) *Channel {
	c := NewWithBaseURL(srv.URL)
	c.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }
	return c
}

type fakeOppoServer struct {
	responses map[string]string
	urls      []*url.URL
	bodies    []string
}

func (f *fakeOppoServer) paths() []string {
	out := make([]string, 0, len(f.urls))
	for _, u := range f.urls {
		out = append(out, u.Path)
	}
	return out
}

func testArtifactFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-OPPO-1.0.0.apk")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRequest(t *testing.T, path string) channel.UploadRequest {
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
		StopAfter:     channel.StageSubmitReview,
	}
}

func appInfoResponse() string {
	return `{"errno":0,"data":{"summary":"一句话","detail_desc":"详细",` +
		`"version_code":"1000","version_name":"1.0.0","audit_status":111,` +
		`"privacy_source_url":"https://p.example.com","ver_second_category_id":"2",` +
		`"ver_third_category_id":"30","icon_url":"https://i.png","pic_url":"https://p.png",` +
		`"test_desc":"","business_username":"","business_email":"","business_mobile":"",` +
		`"copyright_url":"https://c.pdf","electronic_cert_url":"https://e.pdf"}}`
}
