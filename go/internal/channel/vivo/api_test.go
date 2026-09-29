package vivo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
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

// defaultResponses 是各接口的默认成功响应。
var defaultResponses = map[string]string{
	methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":3,"versionCode":"1000","versionName":"1.0.0"}}`,
	methodUploadAPK: `{"code":0,"msg":"ok","data":{"packageName":"com.example.app",` +
		`"serialnumber":"sn-default","versionCode":"1020","versionName":"1.2.0","fileMd5":"md5-default"}}`,
	methodSubmit: `{"code":0,"subCode":0,"msg":"ok"}`,
}

// recordedRequest 是一次被服务端收到的请求。
type recordedRequest struct {
	Method   string
	Path     string
	Query    url.Values
	Header   http.Header
	BodySize int64
	// FormFile 记录 multipart 里的 file 字段（非 multipart 请求为空）
	FormFileName string
	FormFileSize int64
	BodyText     string
}

// newFakeVivo 起一个记录请求的假网关，并按 method 参数返回预设响应。
func newFakeVivo(t *testing.T, responses map[string]string) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var recorded []recordedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			Query:    r.URL.Query(),
			Header:   r.Header.Clone(),
			BodySize: int64(len(body)),
		}
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
			_, params, err := mime.ParseMediaType(ct)
			if err == nil {
				mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
				for {
					part, err := mr.NextPart()
					if err != nil {
						break
					}
					data, _ := io.ReadAll(part)
					if part.FormName() == "file" {
						rec.FormFileName = part.FileName()
						rec.FormFileSize = int64(len(data))
					}
				}
			}
		} else {
			rec.BodyText = string(body)
		}

		mu.Lock()
		recorded = append(recorded, rec)
		mu.Unlock()

		method := r.URL.Query().Get("method")
		resp, ok := responses[method]
		if !ok {
			// 默认给一份完整可用的响应：查询要带 data，上传要带提交所需的全部字段。
			// 只回 {"code":0} 会让 GetAppInfo 因缺 data 失败，测试就测不到后面的流程
			resp = defaultResponses[method]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &recorded
}

func testArtifactFile(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-VIVO-1.0.0.apk")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRequest(path string, stopAfter channel.ReleaseStage) channel.UploadRequest {
	return channel.UploadRequest{
		ArtifactFile: path,
		ArtifactInfo: artifact.Info{
			Path: path, ApplicationID: "com.example.app",
			VersionCode: 1020, VersionName: "1.2.0", Kind: artifact.KindAPK,
		},
		Credentials: channel.NewCredentials(map[string]string{
			ParamAccessKey:    "test-access-key",
			ParamAccessSecret: "test-access-secret",
		}),
		ReleaseParams: channel.ReleaseParams{UpdateDesc: "修复若干问题"},
		Timeouts:      httpx.Default(),
		StopAfter:     stopAfter,
	}
}

func TestUploadFullFlowRequestShapes(t *testing.T) {
	srv, recorded := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":3,"versionCode":"1000","versionName":"1.0.0"}}`,
		methodUploadAPK: `{"code":0,"msg":"ok","data":{"packageName":"com.example.app",` +
			`"serialnumber":"sn-123","versionCode":"1020","versionName":"1.2.0","fileMd5":"abc"}}`,
		methodSubmit: `{"code":0,"subCode":0,"msg":"ok"}`,
	})

	path := testArtifactFile(t, 4096)
	reached, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	reqs := *recorded
	if len(reqs) != 3 {
		t.Fatalf("期望 3 次请求，实际 %d 次：%v", len(reqs), methods(reqs))
	}

	// 请求顺序：先查详情再上传，最后提交。
	// 顺序错了会白传一个上百兆的包才发现凭据无效
	wantMethods := []string{methodGetAppInfo, methodUploadAPK, methodSubmit}
	for i, want := range wantMethods {
		if got := reqs[i].Query.Get("method"); got != want {
			t.Errorf("第 %d 次请求的 method = %q, 期望 %q", i+1, got, want)
		}
	}

	// router/rest 风格：所有接口共用一个路径，业务参数全在 query 上
	for i, r := range reqs {
		if !strings.HasSuffix(r.Path, "/router/rest") && r.Path != "/" {
			t.Errorf("第 %d 次请求路径 = %q", i+1, r.Path)
		}
		// 七个公共参数 + sign 必须齐全，少一个就鉴权失败
		for _, key := range []string{
			"access_key", "timestamp", "sign", "v", "sign_method", "format", "target_app_key",
		} {
			if r.Query.Get(key) == "" {
				t.Errorf("第 %d 次请求缺少签名参数 %s", i+1, key)
			}
		}
		if r.Query.Get("access_key") != "test-access-key" {
			t.Errorf("access_key = %q", r.Query.Get("access_key"))
		}
		if r.Query.Get("v") != "1.0" || r.Query.Get("sign_method") != "HMAC-SHA256" ||
			r.Query.Get("format") != "json" || r.Query.Get("target_app_key") != "developer" {
			t.Errorf("公共参数取值不对：%v", r.Query)
		}
	}

	// 查详情与提交是 GET；上传是 POST multipart
	if reqs[0].Method != http.MethodGet {
		t.Errorf("查详情应为 GET，实际 %s", reqs[0].Method)
	}
	if reqs[2].Method != http.MethodGet {
		// 提交更新保持 GET：换成 POST 表单会改变待签串与服务端的取参方式
		t.Errorf("提交更新应为 GET，实际 %s", reqs[2].Method)
	}
	if reqs[1].Method != http.MethodPost {
		t.Errorf("上传应为 POST，实际 %s", reqs[1].Method)
	}
	if reqs[1].FormFileName != "app-VIVO-1.0.0.apk" {
		t.Errorf("上传的 file 字段文件名 = %q", reqs[1].FormFileName)
	}
	if reqs[1].FormFileSize != 4096 {
		t.Errorf("上传的文件大小 = %d, 期望 4096", reqs[1].FormFileSize)
	}
	if ct := reqs[1].Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("上传的 Content-Type = %q", ct)
	}

	// 提交更新的业务参数
	submit := reqs[2].Query
	for key, want := range map[string]string{
		"packageName": "com.example.app",
		"versionCode": "1020",
		"apk":         "sn-123",
		"fileMd5":     "abc",
		"onlineType":  "1", // 未指定定时上架
		"updateDesc":  "修复若干问题",
	} {
		if got := submit.Get(key); got != want {
			t.Errorf("提交参数 %s = %q, 期望 %q", key, got, want)
		}
	}
	if submit.Get("scheOnlineTime") != "" {
		t.Errorf("非定时上架不应带 scheOnlineTime，实际 %q", submit.Get("scheOnlineTime"))
	}
}

func TestUploadScheduledReleaseIncludesTime(t *testing.T) {
	srv, recorded := newFakeVivo(t, nil)

	path := testArtifactFile(t, 1024)
	req := testRequest(path, channel.StageSubmitReview)
	// 2026-01-15 10:00:00 本地时间
	req.ReleaseParams.OnlineTime = time.Date(2026, 1, 15, 10, 0, 0, 0, time.Local).UnixMilli()

	if _, err := NewWithBaseURL(srv.URL).Upload(context.Background(), req); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	reqs := *recorded
	submit := reqs[len(reqs)-1].Query
	if submit.Get("onlineType") != "2" {
		t.Errorf("onlineType = %q, 定时上架应为 2", submit.Get("onlineType"))
	}
	got := submit.Get("scheOnlineTime")
	if got != "2026-01-15 10:00:00" {
		t.Errorf("scheOnlineTime = %q, 期望 2026-01-15 10:00:00（格式 yyyy-MM-dd HH:mm:ss）", got)
	}
}

func TestUploadStopsAtArtifact(t *testing.T) {
	srv, recorded := newFakeVivo(t, nil)
	path := testArtifactFile(t, 1024)

	reached, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageUploadArtifact))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageUploadArtifact {
		t.Errorf("reached = %v", reached)
	}
	// 只应有「查详情 + 上传」两次，不该提交
	if got := len(*recorded); got != 2 {
		t.Errorf("请求次数 = %d, 期望 2（不应提交）：%v", got, methods(*recorded))
	}
	for _, r := range *recorded {
		if r.Query.Get("method") == methodSubmit {
			t.Error("停在仅上传时不应调用提交接口")
		}
	}
}

func TestUploadRejectsUnsupportedStage(t *testing.T) {
	// vivo 没有草稿态。必须显式报错，不能默默走到送审 ——
	// 调用方以为停在草稿、实际已经提交，是这个功能最坏的失败模式
	_, err := New().Upload(context.Background(), testRequest("/tmp/x.apk", channel.StageCreateDraft))
	if err == nil {
		t.Fatal("期望报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration 错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "草稿") {
		t.Errorf("应说明可停在哪些阶段：%v", err)
	}
}

func TestChannelRejectedSurfacesCode(t *testing.T) {
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":20000,"msg":"access_key 无效","subCode":"20001"}`,
	})
	path := testArtifactFile(t, 1024)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Kind != eperr.KindChannelRejected {
		t.Errorf("Kind = %v", pe.Kind)
	}
	// subCode 更具体，优先作为错误码上报
	if pe.Code != "20001" {
		t.Errorf("Code = %q, 期望 subCode 20001", pe.Code)
	}
	if !strings.Contains(pe.Msg, "access_key 无效") {
		t.Errorf("应带上渠道的错误描述：%s", pe.Msg)
	}
}

func TestSubCodeAloneTriggersFailure(t *testing.T) {
	// 上游的 get("subCode").asString 在只有 code 没有 subCode 时抛 NPE，
	// 把本该抛出的业务异常顶掉了。这里两个码都要空安全地判定
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":3}}`,
		methodUploadAPK:  `{"code":0,"subCode":70006,"msg":"文件校验失败"}`,
	})
	path := testArtifactFile(t, 1024)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("subCode 非 0 应当失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Code != "70006" {
		t.Errorf("期望 subCode 70006，实际 %v", err)
	}
}

func TestNumericAndStringCodesBothWork(t *testing.T) {
	// vivo 这两个字段的 JSON 类型不稳定：限流走数字、部分鉴权失败走字符串。
	// 声明成具体类型会在反序列化阶段就失败，于是又把真实错误码顶掉
	for _, body := range []string{
		`{"code":20000,"msg":"限流"}`,
		`{"code":"20000","msg":"限流"}`,
	} {
		srv, _ := newFakeVivo(t, map[string]string{methodGetAppInfo: body})
		path := testArtifactFile(t, 512)
		_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
			testRequest(path, channel.StageSubmitReview))
		if err == nil {
			t.Fatalf("%s 应当失败", body)
		}
		var pe *eperr.Error
		if !errors.As(err, &pe) || pe.Code != "20000" {
			t.Errorf("%s: 期望 code 20000，实际 %v", body, err)
		}
	}
}

func TestMissingBothCodesIsFailureNotSuccess(t *testing.T) {
	// 两个码都缺失说明响应不是 vivo 的正常信封。判成成功会让 Submit 这种
	// 没有后续 data 校验的调用谎报发版成功 —— 比抛 NPE 更危险
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"data":{"status":3,"versionCode":1,"versionName":"1.0"}}`,
	})
	path := testArtifactFile(t, 512)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("既无 code 也无 subCode 时应按失败处理")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
}

func TestSubmitFailureIsMarkedAtSubmissionPoint(t *testing.T) {
	// 提交更新是送审点：即便错误看起来是普通网络问题，也不能标记为可重试，
	// 因为服务端可能已受理而响应丢失
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":3,"versionCode":1,"versionName":"1.0"}}`,
		methodUploadAPK: `{"code":0,"msg":"ok","data":{"packageName":"com.example.app",` +
			`"serialnumber":"sn","versionCode":"1020","fileMd5":"m"}}`,
		methodSubmit: `<html>502 Bad Gateway</html>`,
	})
	path := testArtifactFile(t, 512)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v, 送审点的失败必须标记为 AtOrAfterSubmission", pe.Phase)
	}
	if pe.Retryable() {
		t.Error("送审点的失败绝不能标记为可重试")
	}
	if !strings.Contains(pe.Msg, "确认") {
		t.Errorf("提示应引导先到后台确认：%s", pe.Msg)
	}
}

func TestPreSubmitFailureStaysRetryable(t *testing.T) {
	// 同样是无法解析的响应，发生在查详情阶段就仍是可重试的 ——
	// 判断依据是「结果是否确定」，不是「错误看起来是否可恢复」
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `<html>502</html>`,
	})
	path := testArtifactFile(t, 512)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhasePreSubmission {
		t.Errorf("Phase = %v, 送审前的失败应保持 PreSubmission", pe.Phase)
	}
	// 这里不断言 Retryable：无法解析的响应属于 ProtocolMismatch，
	// 无论发生在哪个阶段都不该自动重试（可重试的只有 Network 与 Unknown）。
	// 本测试的要点是 phase 的差别 —— 同样的错误发生在送审点之后会额外带上
	// 「先到后台确认」的提示，并因此变成不可重试
	if strings.Contains(pe.Msg, "确认该版本是否已提交成功") {
		t.Errorf("送审前的失败不应带送审点提示：%s", pe.Msg)
	}
}

func TestQueryMarketMapsStatus(t *testing.T) {
	cases := []struct {
		status int
		want   channel.ReviewState
	}{
		{1, channel.ReviewDraft},
		{2, channel.ReviewUnderReview},
		{3, channel.ReviewOnline},
		{4, channel.ReviewRejected},
		{99, channel.ReviewUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv, _ := newFakeVivo(t, map[string]string{
				methodGetAppInfo: fmt.Sprintf(
					`{"code":0,"msg":"ok","data":{"status":%d,"versionCode":"1000","versionName":"1.0.0"}}`,
					tc.status),
			})
			info, err := NewWithBaseURL(srv.URL).QueryMarket(context.Background(), channel.MarketQuery{
				ApplicationID: "com.example.app",
				Credentials: channel.NewCredentials(map[string]string{
					ParamAccessKey: "k", ParamAccessSecret: "s",
				}),
				Timeouts: httpx.Default(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if info.ReviewState != tc.want {
				t.Errorf("status=%d 映射为 %v, 期望 %v", tc.status, info.ReviewState, tc.want)
			}
			if info.RawState != fmt.Sprint(tc.status) {
				t.Errorf("RawState = %q, 应保留原始值便于排查新增状态码", info.RawState)
			}
		})
	}
}

func TestQueryMarketWithoutVersionDoesNotFabricate(t *testing.T) {
	// 商店里只有尚未上传 APK 的草稿时 vivo 不返回版本号。
	// 此时应当是「没有版本信息」，而不是伪造一个 0 ——
	// 0 会让版本号比对得出「待提交版本更高」的错误结论
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":1}}`,
	})
	info, err := NewWithBaseURL(srv.URL).QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials:   channel.NewCredentials(map[string]string{ParamAccessKey: "k", ParamAccessSecret: "s"}),
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.LastVersion != nil {
		t.Errorf("LastVersion = %v, 期望 nil", info.LastVersion)
	}
}

func TestUploadResultMissingFieldsIsProtocolError(t *testing.T) {
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":3}}`,
		// 缺 serialnumber 与 fileMd5
		methodUploadAPK: `{"code":0,"msg":"ok","data":{"packageName":"com.example.app","versionCode":"1020"}}`,
	})
	path := testArtifactFile(t, 512)

	_, err := NewWithBaseURL(srv.URL).Upload(context.Background(),
		testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
	// 提示里应列出缺失的字段名，否则无从判断是接口变了还是我们解析错了
	for _, field := range []string{"serialnumber", "fileMd5"} {
		if !strings.Contains(pe.Msg, field) {
			t.Errorf("错误信息应列出缺失字段 %s，实际：%s", field, pe.Msg)
		}
	}
}

func TestCancelledContextAbortsUpload(t *testing.T) {
	srv, _ := newFakeVivo(t, nil)
	path := testArtifactFile(t, 1024)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewWithBaseURL(srv.URL).Upload(ctx, testRequest(path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("已取消的 ctx 应当导致失败")
	}
	// 取消必须能被上层识别为取消而不是失败。上游用 catch(Throwable) 把两者混为一谈，
	// 界面显示「上传失败」并给出重试按钮
	if !errors.Is(err, context.Canceled) {
		var pe *eperr.Error
		if errors.As(err, &pe) && pe.Phase == eperr.PhaseAtOrAfterSubmission {
			t.Errorf("取消不应被标记为送审点失败：%v", err)
		}
	}
}

func TestCapabilitiesDeclaration(t *testing.T) {
	caps := New().Capabilities()
	want := []channel.ReleaseStage{channel.StageUploadArtifact, channel.StageSubmitReview}
	if len(caps.SupportedStages) != len(want) {
		t.Fatalf("SupportedStages = %v", caps.SupportedStages)
	}
	for i := range want {
		if caps.SupportedStages[i] != want[i] {
			t.Errorf("SupportedStages[%d] = %v, 期望 %v", i, caps.SupportedStages[i], want[i])
		}
	}
	if caps.SupportedStages[len(want)-1] != channel.StageSubmitReview {
		t.Error("阶段列表应以 SubmitReview 结尾")
	}
	if caps.RiskLevel != channel.RiskCritical {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试")
	}
	if !caps.RequiresExplicitConfirmation {
		t.Error("送审必须要求显式确认")
	}
	// 已实测的范围必须写清楚，且不得暗示送审也验证过
	if caps.Evidence != channel.EvidenceVerifiedInProduction {
		t.Errorf("Evidence = %v", caps.Evidence)
	}
	if !strings.Contains(caps.VerifiedScope, "送审未验证") {
		t.Errorf("VerifiedScope 必须写明送审未验证：%s", caps.VerifiedScope)
	}
	if caps.SupportedStages[1] == channel.StageCreateDraft {
		t.Error("vivo 没有草稿态")
	}
}

func TestParamsDeclaration(t *testing.T) {
	params := New().Params()
	if len(params) != 2 {
		t.Fatalf("参数个数 = %d", len(params))
	}
	for _, p := range params {
		if !p.Required {
			t.Errorf("%s 应为必填", p.Name)
		}
		if p.Description == "" {
			t.Errorf("%s 缺少说明", p.Name)
		}
	}
	if params[0].Name != ParamAccessKey || params[1].Name != ParamAccessSecret {
		t.Errorf("参数名不对：%v %v", params[0].Name, params[1].Name)
	}
}

func methods(reqs []recordedRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Query.Get("method"))
	}
	return out
}

// TestAppInfoRetriesOnBusyCode 覆盖 vivo 的「应用处理中，请勿重复提交」（11010）。
//
// 回归背景：实测该码不是「有提交在进行」，而是**按包名的查询频率限制** ——
// 同一包名数秒内重复查 app.query.details 即触发，换包名不受影响，静默几秒恢复。
// 而发布流程会连续查询（校验一次、上传相关再查），必然撞上，
// 此前它直接把整个发布任务判为失败。
//
// 这里用一个先返回 11010、后返回成功的假服务，断言重试真的发生且最终成功。
func TestAppInfoRetriesOnBusyCode(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// 第一次：频率限制
			_, _ = w.Write([]byte(`{"code":0,"subCode":11010,"subMsg":"应用处理中，请勿重复提交"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"subCode":0,"msg":"ok",` +
			`"data":{"status":3,"versionCode":"11","versionName":"1.0.11"}}`))
	}))
	defer srv.Close()

	api := NewAPI("ak", "as", srv.Client(), srv.URL+"/router/rest")
	info, err := getAppInfoWithRetry(context.Background(), api, "com.example.app")
	if err != nil {
		t.Fatalf("11010 应被重试，最终成功；实际失败: %v", err)
	}
	if info.VersionCode == nil || int64(*info.VersionCode) != 11 {
		t.Errorf("重试后应拿到真实数据，实际 %+v", info)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Errorf("应至少调用 2 次（首次限流 + 重试），实际 %d 次", calls)
	}
}

// TestAppInfoDoesNotRetryOtherErrors 确保只重试 11010。
// 其他业务错误立即失败，不浪费时间也不掩盖问题。
func TestAppInfoDoesNotRetryOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"subCode":10004,"msg":"没有接口访问权限"}`))
	}))
	defer srv.Close()

	api := NewAPI("ak", "as", srv.Client(), srv.URL+"/router/rest")
	_, err := getAppInfoWithRetry(context.Background(), api, "com.example.app")
	if err == nil {
		t.Fatal("非 11010 的错误应直接失败")
	}
	if !strings.Contains(err.Error(), "没有接口访问权限") {
		t.Errorf("错误应保留渠道原因：%v", err)
	}
}

// ---- 审核意见 ----

// TestQueryMarketCarriesUnPassReason 验证 vivo 的审核不通过原因被带出来。
//
// 字段名来自官方《查询详细信息》文档：data.unPassReason「审核不通过原因」。
func TestQueryMarketCarriesUnPassReason(t *testing.T) {
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{
			"status":4,"versionCode":"1000","versionName":"1.0.0",
			"unPassReason":"应用名称与软著不一致，请修改后重新提交"}}`,
	})

	info, err := NewWithBaseURL(srv.URL).QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials: channel.NewCredentials(map[string]string{
			ParamAccessKey: "k", ParamAccessSecret: "s",
		}),
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewRejected {
		t.Errorf("status=4 应映射为审核被拒，实际 %v", info.ReviewState)
	}
	if info.Review == nil {
		t.Fatal("应带上审核不通过原因")
	}
	if !strings.Contains(info.Review.Opinion, "软著不一致") {
		t.Errorf("原因内容不对：%q", info.Review.Opinion)
	}
}

// TestQueryMarketWithoutUnPassReason 覆盖字段缺失。
//
// vivo 官方把 unPassReason 标为「非必填」，且成功响应示例里根本没出现它 ——
// 因此不能假设被拒时就一定有原因。缺了就缺了，不伪造内容。
func TestQueryMarketWithoutUnPassReason(t *testing.T) {
	srv, _ := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":4,"versionCode":"1000","versionName":"1.0.0"}}`,
	})

	info, err := NewWithBaseURL(srv.URL).QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials: channel.NewCredentials(map[string]string{
			ParamAccessKey: "k", ParamAccessSecret: "s",
		}),
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewRejected {
		t.Errorf("status=4 应映射为审核被拒，实际 %v", info.ReviewState)
	}
	if info.Review != nil {
		t.Errorf("渠道未返回原因时不应留下空壳：%+v", info.Review)
	}
}

// TestQueryMarketIgnoresStageAuditOpinion 记录一个容易踩错的字段。
//
// vivo 的 app.query.stage.details（分阶段发布）里也有个 auditOpinion，
// 名字看着像审核意见，官方定义实为「催撤审 1-催审 2-撤审 默认为空」——
// 与驳回原因无关。我们只读 app.query.details 的 unPassReason，不碰那个字段。
func TestQueryMarketIgnoresStageAuditOpinion(t *testing.T) {
	srv, recorded := newFakeVivo(t, map[string]string{
		methodGetAppInfo: `{"code":0,"msg":"ok","data":{"status":4,"versionCode":"1000"}}`,
	})

	if _, err := NewWithBaseURL(srv.URL).QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.app",
		Credentials: channel.NewCredentials(map[string]string{
			ParamAccessKey: "k", ParamAccessSecret: "s",
		}),
		Timeouts: httpx.Default(),
	}); err != nil {
		t.Fatal(err)
	}

	// 状态查询只该调 app.query.details，不该碰分阶段发布那个接口
	for _, r := range *recorded {
		if m := r.Query.Get("method"); strings.Contains(m, "stage") {
			t.Errorf("不应调用分阶段发布接口（其 auditOpinion 不是驳回原因）：%s", m)
		}
	}
}
