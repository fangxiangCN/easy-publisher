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
			`"copyright_url":"https://c.pdf","electronic_cert_url":"https://e.pdf",` +
			`"app_name":"测试应用","age_level":"12","adaptive_equipment":"4"}}`,
		pathUploadURL: `{"errno":0,"data":{"upload_url":"` + "" + `","sign":"once-sign"}}`,
		pathSubmit:    `{"errno":0}`,
		// 提交是异步的：errno=0 只代表任务入队，成功后需轮询到 task_state=2
		pathTaskState: `{"errno":0,"data":{"pkg_name":"com.example.app","version_code":"1000","task_state":"2","err_msg":""}}`,
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
	// 末位是 task-state：app/upd 是异步接口，必须轮询任务结果才知道版本有没有建成
	want := []string{pathToken, pathAppInfo, pathUploadURL, "/upload", pathSubmit, pathTaskState}
	if len(paths) != len(want) {
		t.Fatalf("请求次数 = %d, 期望 %d：%v", len(paths), len(want), paths)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("第 %d 个请求路径 = %q, 期望 %q（顺序：token → app/info → upload-url → 上传 → 提交 → 轮询任务）",
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
		// 提交是异步的：errno=0 只代表任务入队，成功后需轮询到 task_state=2
		pathTaskState: `{"errno":0,"data":{"pkg_name":"com.example.app","version_code":"1000","task_state":"2","err_msg":""}}`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	if _, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	// app/upd 是全量更新语义：漏任何一个字段就会被清空或被拒
	submit, ok := fake.bodyFor(pathSubmit)
	if !ok {
		t.Fatal("未找到提交请求")
	}
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
		pathToken:     `{"errno":0,"data":{"access_token":"tok"}}`,
		pathAppInfo:   strings.Replace(appInfoResponse(), `"copyright_url":"https://c.pdf"`, `"copyright_url":""`, 1),
		"/upload":     `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
		pathSubmit:    `{"errno":0}`,
		pathTaskState: `{"errno":0,"data":{"task_state":"2"}}`,
	})
	fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

	path := testArtifactFile(t)
	if _, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, path)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	submitBody, _ := fake.bodyFor(pathSubmit)
	form, _ := url.ParseQuery(submitBody)
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
		// 未知状态码归为 Unknown，**不是** UnderReview。
		//
		// 这曾经是 UnderReview，后果不只是显示错：canSubmit 由「是否审核中」推导，
		// 而 PublishPolicy 会因此拒绝提交 —— 用户会看到「渠道正在审核中，不能提交
		// 新版本」，而真实情况可能是被拒了、应该重新提交。
		//
		// OPPO 的审核状态取值远不止这两个（第三方文档镜像提到还有测试不通过、
		// 运营打回、资质审核不通过等），但那些来源已无法从官方核实。
		// 不认识就不猜：Unknown 且不阻断提交，让渠道自己拒绝比我们替它下结论准确。
		{1, channel.ReviewUnknown},
		{99, channel.ReviewUnknown},
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
			// 未知状态不该阻断提交：真正的拒绝理由由渠道给出，比我们猜准
			if tc.want == channel.ReviewUnknown && !info.CanSubmit {
				t.Errorf("audit_status=%d 是未知状态，不应阻断提交", tc.status)
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
		`"copyright_url":"https://c.pdf","electronic_cert_url":"https://e.pdf",` +
		// 发布版本接口的必传字段，缺少会让异步任务静默失败
		`"app_name":"测试应用","age_level":"12","adaptive_equipment":"4"}}`
}

// TestSubmitPollsTaskState 覆盖 app/upd 是异步接口这一点。
//
// 回归背景：app/upd 返回 errno=0 只代表任务入队。若缺必传参数（如 app_name），
// 任务会入队后静默失败 —— 而旧实现直接报告「已提交新版本」，线上版本号毫无变化。
// 用户看到的是成功，实际什么都没发生，比明确报错更难排查。
// 线上就是这么丢掉一次提交的，因此把「必须轮询到 task_state=2 才算成功」固化下来。
func TestSubmitPollsTaskState(t *testing.T) {
	t.Run("任务处理成功才算提交成功", func(t *testing.T) {
		srv, fake := newFakeOppo(t, map[string]string{
			pathToken:     `{"errno":0,"data":{"access_token":"tok"}}`,
			pathAppInfo:   appInfoResponse(),
			"/upload":     `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
			pathSubmit:    `{"errno":0,"data":{"success":true}}`,
			pathTaskState: `{"errno":0,"data":{"pkg_name":"com.example.app","version_code":"1000","task_state":"2","err_msg":""}}`,
		})
		fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

		stage, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, testArtifactFile(t)))
		if err != nil {
			t.Fatalf("任务成功时不应报错: %v", err)
		}
		if stage != channel.StageSubmitReview {
			t.Errorf("stage = %v, 期望 StageSubmitReview", stage)
		}
		// 必须真的查过任务状态
		queried := false
		for _, u := range fake.urls {
			if u.Path == pathTaskState {
				queried = true
				break
			}
		}
		if !queried {
			t.Error("未轮询任务状态：errno=0 只代表入队，不查就无法知道版本是否真的创建")
		}
	})

	t.Run("任务处理失败必须报错", func(t *testing.T) {
		srv, fake := newFakeOppo(t, map[string]string{
			pathToken:   `{"errno":0,"data":{"access_token":"tok"}}`,
			pathAppInfo: appInfoResponse(),
			"/upload":   `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
			pathSubmit:  `{"errno":0,"data":{"success":true}}`,
			// 这是线上真实见过的形态：入队成功，任务随后失败
			pathTaskState: `{"errno":0,"data":{"pkg_name":"com.example.app","version_code":"1000",` +
				`"task_state":"3","err_msg":"二级分类ID不能为空"}}`,
		})
		fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

		_, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, testArtifactFile(t)))
		if err == nil {
			t.Fatal("任务处理失败时必须报错，否则会把未生效的提交当成成功")
		}
		if !strings.Contains(err.Error(), "二级分类ID不能为空") {
			t.Errorf("错误信息应带上渠道给出的原因：%v", err)
		}
	})

	t.Run("提交参数含文档要求的必传字段", func(t *testing.T) {
		srv, fake := newFakeOppo(t, map[string]string{
			pathToken:     `{"errno":0,"data":{"access_token":"tok"}}`,
			pathAppInfo:   appInfoResponse(),
			"/upload":     `{"errno":0,"data":{"url":"https://cdn/app.apk","md5":"m"}}`,
			pathSubmit:    `{"errno":0,"data":{"success":true}}`,
			pathTaskState: `{"errno":0,"data":{"task_state":"2"}}`,
		})
		fake.responses[pathUploadURL] = `{"errno":0,"data":{"upload_url":"` + srv.URL + `/upload","sign":"s"}}`

		if _, err := newTestChannel(srv).Upload(context.Background(), testRequest(t, testArtifactFile(t))); err != nil {
			t.Fatal(err)
		}
		var submitBody string
		for i, u := range fake.urls {
			if u.Path == pathSubmit {
				submitBody = fake.bodies[i]
				break
			}
		}
		if submitBody == "" {
			t.Fatal("未发出提交请求")
		}
		form, err := url.ParseQuery(submitBody)
		if err != nil {
			t.Fatalf("提交体不是 form 编码: %v", err)
		}
		// 文档 id=10999 标注必传；漏传会让异步任务静默失败
		for _, key := range []string{"app_name", "age_level", "adaptive_equipment"} {
			if form.Get(key) == "" {
				t.Errorf("提交参数缺少文档标注的必传字段 %s", key)
			}
		}
	})
}

// TestReviewFeedbackFromRealResponse 覆盖审核意见的组装。
//
// 数据取自 2026-09-29 的真实响应（gwy 被拒那次）。要点：
//
//  1. `refuse_reason` 把审核员的**测试环境**拼在最前面，原样展示会误导使用者
//     以为「测试机型：find x9」是需要处理的问题 —— 必须从 with_sugg 里挑。
//  2. `refuse_advice` 是完整的一段修改指引，比逐条 advice 更全（逐条里只有
//     最后一条带 advice）。
//  3. `refuse_file` 是审核员附件（实测为 zip）。
func TestReviewFeedbackFromRealResponse(t *testing.T) {
	raw := `{"errno":0,"data":{` +
		`"audit_status":"444","version_code":"45","version_name":"2026.09.28",` +
		`"refuse_reason":"测试机型：find x9 ；,Android版本：16；,软件版本：PLJ110_16.0.5.701(CN01B60P01)；,` +
		`1、您的应用内存在付费会员项目，需在会员订阅支付界面，以醒目的方式提示用户“会员服务协议”。",` +
		`"refuse_advice":"您需在用户付费界面明确展示“会员服务协议”相关文档链接，以供用户阅读。",` +
		`"refuse_file":"https://audit-platform-cn.heytapimage.com/content-audit/202609/29/xxx.zip",` +
		`"refuse_reason_with_sugg":[` +
		`{"reason":"测试机型：find x9 ；","advice":""},` +
		`{"reason":"Android版本：16；","advice":""},` +
		`{"reason":"软件版本：PLJ110_16.0.5.701(CN01B60P01)；","advice":""},` +
		`{"reason":"1、您的应用内存在付费会员项目，需在会员订阅支付界面，以醒目的方式提示用户“会员服务协议”。",` +
		`"advice":"您需在用户付费界面明确展示“会员服务协议”相关文档链接，以供用户阅读。"}]}}`

	var resp envelope
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	var appInfo AppInfo
	if err := unmarshalData(raw, &appInfo); err != nil {
		t.Fatal(err)
	}
	info := appInfo.ToMarketInfo()

	if info.ReviewState != channel.ReviewRejected {
		t.Errorf("audit_status=444 应判为被拒，实际 %v", info.ReviewState)
	}
	if info.Review == nil || info.Review.Empty() {
		t.Fatal("有审核意见却未带出")
	}

	opinion := info.Review.Opinion
	// 环境信息不应出现在意见里
	for _, noise := range []string{"测试机型", "Android版本", "软件版本"} {
		if strings.Contains(opinion, noise) {
			t.Errorf("意见里混入了测试环境信息 %q：\n%s", noise, opinion)
		}
	}
	// 真正的问题与建议要在
	if !strings.Contains(opinion, "会员服务协议") {
		t.Errorf("真实问题缺失：%s", opinion)
	}
	if !strings.Contains(opinion, "建议：") {
		t.Errorf("应把 advice 与 reason 配成一条：%s", opinion)
	}

	// refuse_advice 不重复成 Note：实测它就是最后一条 with_sugg 的 advice，
	// 两条并排展示完全重复。它的价值在 with_sugg 缺失时作为回退（见下一个测试）。
	if len(info.Review.Notes) != 0 {
		t.Errorf("with_sugg 可用时不应再产出 Note（内容重复）：%+v", info.Review.Notes)
	}
	// 附件要带出
	if len(info.Review.Attachments) != 1 || !strings.HasSuffix(info.Review.Attachments[0], ".zip") {
		t.Errorf("refuse_file 应作为附件带出：%+v", info.Review.Attachments)
	}
}

// TestReviewFeedbackFallsBackToPlainReason 确认 with_sugg 缺失时仍有内容可用。
//
// 此时 refuse_advice 是唯一的建议来源，应当作为 Note 保留。
func TestReviewFeedbackFallsBackToPlainReason(t *testing.T) {
	appInfo := AppInfo{
		RefuseReason: "资源下架理由外部",
		RefuseAdvice: "请补充版权证明材料",
	}
	fb := appInfo.reviewFeedback()
	if fb.Empty() {
		t.Fatal("with_sugg 缺失时应回退到 refuse_reason，不能什么都不给")
	}
	if fb.Opinion != "资源下架理由外部" {
		t.Errorf("Opinion = %q", fb.Opinion)
	}
	if len(fb.Notes) != 1 || fb.Notes[0].Opinion != "请补充版权证明材料" {
		t.Errorf("回退路径下 refuse_advice 应成为 Note：%+v", fb.Notes)
	}
}

// TestReviewFeedbackEmptyWhenClean 确认无意见时返回空反馈（不产生噪音）。
func TestReviewFeedbackEmptyWhenClean(t *testing.T) {
	// 真实场景：应用已上线，refuse_* 全是空串 —— 此时不应展示任何审核意见
	appInfo := AppInfo{RefuseReason: "", RefuseAdvice: "", RefuseFile: ""}
	if fb := appInfo.reviewFeedback(); !fb.Empty() {
		t.Errorf("干净状态下不应有反馈：%+v", fb)
	}
}

// TestReviewFeedbackIgnoresEnvironmentOnlyReason 覆盖一个实测到的易错点。
//
// **应用正常上线时 refuse_reason 也有值**，内容全是审核员的测试环境：
//
//	"测试机型：OPPO Find X9；,Android版本：16.0.5；,软件版本：PLJ110；"
//
// 若原样带出，「已上架」的应用也会挂一条「审核意见」—— 纯噪音，还会让人
// 误以为上线失败。因此仅当串里确有非环境信息时才作为意见展示。
func TestReviewFeedbackIgnoresEnvironmentOnlyReason(t *testing.T) {
	// 真实场景：civilian 已上线（audit_status=111），refuse_reason 只有环境信息
	appInfo := AppInfo{
		RefuseReason: "测试机型：OPPO Find X9；,Android版本：16.0.5；,软件版本：PLJ110；",
	}
	fb := appInfo.reviewFeedback()
	if !fb.Empty() {
		t.Errorf("只有测试环境信息时不应产出审核意见，实际：%+v", fb)
	}
}

// TestReviewFeedbackKeepsRealIssueInRawReason 确认混合内容时不会把真问题一起滤掉。
func TestReviewFeedbackKeepsRealIssueInRawReason(t *testing.T) {
	// with_sugg 缺失，只能从原始串判断；环境信息在前、真问题在后
	appInfo := AppInfo{
		RefuseReason: "测试机型：find x9 ；,Android版本：16；,1、付费会员界面需展示会员服务协议。",
	}
	fb := appInfo.reviewFeedback()
	if fb.Empty() {
		t.Fatal("串里含有真实问题时不应判为空")
	}
	if !strings.Contains(fb.Opinion, "会员服务协议") {
		t.Errorf("真实问题应保留：%q", fb.Opinion)
	}
}
