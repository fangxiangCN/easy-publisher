package harmony

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	Body   []byte
}

type fakeHarmony struct {
	// srvURL 是假网关地址，测试构造分片 URL 时要用
	srvURL string

	mu        sync.Mutex
	responses map[string]string
	requests  []recorded
	// partSeq 是分片上传的递增序号，用于生成唯一 ETag
	partSeq int
	// partFailN 让第 N 次分片上传返回失败（0 表示不失败）
	partFailN int
	// submitFailTimes 让前 N 次送审返回 204144660
	submitFailTimes int
	submitCalls     int
}

func (f *fakeHarmony) set(path, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = resp
}

func (f *fakeHarmony) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeHarmony) paths() []string {
	out := []string{}
	for _, r := range f.all() {
		out = append(out, r.Path)
	}
	return out
}

func (f *fakeHarmony) count(suffix string) int {
	n := 0
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

func (f *fakeHarmony) mustFind(t *testing.T, suffix string) recorded {
	t.Helper()
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			return r
		}
	}
	t.Fatalf("没找到路径以 %q 结尾的请求，实际：%v", suffix, f.paths())
	return recorded{}
}

const (
	initPath    = "/api/publish/v2/upload/multipart/init"
	partsPath   = "/api/publish/v2/upload/multipart/parts"
	composePath = "/api/publish/v2/upload/multipart/compose"
	partUpload  = "/part-upload"
)

// newFakeHarmony 起假网关并返回渠道实例。
//
// 分片大小设为 64 字节，这样 200 字节的 fixture 会被切成 4 片，
// 足以覆盖「有余数」的分片逻辑。
func newFakeHarmony(t *testing.T) (*Channel, *fakeHarmony) {
	t.Helper()
	fake := &fakeHarmony{responses: map[string]string{}}

	// srvURL 先声明后赋值：闭包需要它，而它只能从 NewTLSServer 的返回值取。
	// 闭包捕获的是变量本身，请求到达时已经赋好值
	var srvURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
			Header: r.Header.Clone(), Body: body,
		})
		failN := fake.partFailN
		submitFailTimes := fake.submitFailTimes
		fake.mu.Unlock()

		switch r.URL.Path {
		case partUpload:
			fake.mu.Lock()
			fake.partSeq++
			seq := fake.partSeq
			fake.mu.Unlock()
			// partSeq 是分片序号，用它判断「第几片失败」，
			// 不要用请求总数（那里面还含 token/init/parts）
			if failN > 0 && seq == failN {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			// ETag 带引号，返回时也必须原样带回
			w.Header().Set("ETag", `"etag-`+strconv.Itoa(seq)+`"`)
			w.WriteHeader(http.StatusOK)
			return

		case "/api/publish/v3/app-submit":
			fake.mu.Lock()
			fake.submitCalls++
			n := fake.submitCalls
			fake.mu.Unlock()
			if n <= submitFailTimes {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ret":{"code":204144660,"msg":"package not compiled"}}`))
				return
			}
		}

		// parts 响应按本次请求里的分片数动态生成。
		//
		// 不能硬编码片数：zip 的文件头与中央目录会让 .app 的实际大小超出
		// pack.info 的字节数，片数与预期对不上
		if r.URL.Path == partsPath {
			fake.mu.Lock()
			explicit, hasExplicit := fake.responses[r.URL.Path]
			fake.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if hasExplicit {
				_, _ = w.Write([]byte(explicit))
				return
			}
			var descriptors map[string]json.RawMessage
			_ = json.Unmarshal(body, &descriptors)
			_, _ = w.Write([]byte(partsResponse(srvURL, len(descriptors))))
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
			_, _ = w.Write([]byte(`{"ret":{"code":404,"msg":"unexpected ` + r.URL.Path + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	srvURL = srv.URL
	fake.srvURL = srvURL
	ch := NewWithBaseURL(srvURL + "/")
	ch.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }
	interval := time.Millisecond
	retries := 3
	ch.retryInterval = &interval
	ch.maxRetries = &retries

	fake.set(initPath, `{"ret":{"code":0},"objectId":"obj-1","nspUploadId":"up-1",`+
		`"nspPartMinSize":64}`)
	fake.set(composePath, `{"ret":{"code":0}}`)
	fake.set("/api/publish/v3/app-package-info", `{"ret":{"code":0},"packageId":"pkg-777"}`)

	return ch, fake
}

// expectedParts 按实际文件大小与分片大小算期望片数。
//
// 不硬编码：zip 的文件头与中央目录会让 .app 的实际大小超出 pack.info 的字节数
func expectedParts(t *testing.T, path string) int {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	n := int((st.Size() + 64 - 1) / 64)
	if n < 1 {
		n = 1
	}
	return n
}

// partsResponse 按分片数生成 parts 响应。
//
// 键名是 additionalPropN —— Swagger 占位符风格，必须与服务端的期望逐字一致。
func partsResponse(srvURL string, partCount int) string {
	var b strings.Builder
	b.WriteString(`{"ret":{"code":0},"uploadInfoMap":{`)
	for i := 1; i <= partCount; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"additionalProp%d":{"url":"%s%s","method":"PUT",`+
			`"headers":{"x-obs-auth":"sig"},"partObjectId":"po-%d"}`,
			i, srvURL, partUpload, i)
	}
	b.WriteString("}}")
	return b.String()
}

var defaults = map[string]string{
	"/api/oauth2/v1/token":       `{"access_token":"tok-harmony"}`,
	"/api/publish/v3/app-submit": `{"ret":{"code":0}}`,
}

// appPack 构造一个 200 字节的 .app（zip 内含 pack.info）。
func appPack(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-HARMONY-1.0.0.app")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("pack.info")
	if err != nil {
		t.Fatal(err)
	}
	pack := map[string]any{"summary": map[string]any{
		"app": map[string]any{
			"bundleName": "com.example.harmony",
			"version":    map[string]any{"code": 1000012, "name": "1.0.12"},
		},
	}}
	data, _ := json.Marshal(pack)
	_, _ = w.Write(data)
	// 补齐到 200 字节，便于切成 4 片（64/64/64/8）
	if pad := 200 - len(data); pad > 0 {
		w2, _ := zw.Create("entry.hap")
		_, _ = w2.Write(bytes.Repeat([]byte("x"), pad))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRequest(t *testing.T, path string, stage channel.ReleaseStage) channel.UploadRequest {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return channel.UploadRequest{
		ArtifactFile: path,
		ArtifactInfo: artifact.Info{
			Path: path, ApplicationID: "com.example.harmony",
			VersionCode: 1000012, VersionName: "1.0.12",
			SizeBytes: st.Size(), Kind: artifact.KindHarmonyAppPack,
		},
		Credentials: channel.NewCredentials(map[string]string{
			ParamClientID: "test-client-id", ParamClientSecret: "test-client-secret",
			ParamAppID: "harmony-app-1",
		}),
		ReleaseParams: channel.ReleaseParams{UpdateDesc: "修复若干问题"},
		Timeouts:      httpx.Default(),
		StopAfter:     stage,
	}
}

// ---- 全流程 ----

func TestUploadFullFlowCallOrder(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)

	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	paths := fake.paths()
	// token → init → parts → 4 个分片 → compose → 关联草稿 → 送审
	if paths[0] != "/api/oauth2/v1/token" {
		t.Errorf("第一步应为 token，实际 %s", paths[0])
	}
	if paths[1] != initPath || paths[2] != partsPath {
		t.Errorf("init/parts 顺序不对：%v", paths[:3])
	}
	wantParts := expectedParts(t, path)
	if got := fake.count(partUpload); got != wantParts {
		t.Errorf("分片上传次数 = %d, 期望 %d", got, wantParts)
	}
	if paths[len(paths)-3] != composePath {
		t.Errorf("compose 应在分片之后，实际顺序：%v", paths)
	}
	if paths[len(paths)-2] != "/api/publish/v3/app-package-info" {
		t.Errorf("关联草稿应在 compose 之后：%v", paths)
	}
	if paths[len(paths)-1] != "/api/publish/v3/app-submit" {
		t.Errorf("送审应最后：%v", paths)
	}
}

func TestInitCarriesFixedParams(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	init := fake.mustFind(t, "multipart/init")
	if init.Method != http.MethodPost {
		t.Errorf("init 方法 = %s, 期望 POST", init.Method)
	}
	// 这四个参数取值与参照实现一致，不要改动
	want := map[string]string{
		"appId":       "harmony-app-1",
		"fileName":    "app-HARMONY-1.0.0.app",
		"contentType": InitContentType,
		"fileType":    "1",
		"releaseType": "1",
	}
	for k, v := range want {
		if got := init.Query.Get(k); got != v {
			t.Errorf("init 参数 %s = %q, 期望 %q", k, got, v)
		}
	}
	if got := init.Header.Get("client_id"); got != "test-client-id" {
		t.Errorf("init 的 client_id 头 = %q", got)
	}
	if got := init.Header.Get("Authorization"); got != "Bearer tok-harmony" {
		t.Errorf("init 的 Authorization 头 = %q", got)
	}
}

func TestPartsRequestHasSHA256PerPart(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	parts := fake.mustFind(t, "multipart/parts")
	if got := parts.Query.Get("objectId"); got != "obj-1" {
		t.Errorf("objectId = %q", got)
	}
	if got := parts.Query.Get("nspUploadId"); got != "up-1" {
		t.Errorf("nspUploadId = %q", got)
	}

	var descriptors map[string]PartDescriptor
	if err := json.Unmarshal(parts.Body, &descriptors); err != nil {
		t.Fatalf("请求体解析失败：%s", parts.Body)
	}
	wantParts := expectedParts(t, path)
	if len(descriptors) != wantParts {
		t.Fatalf("分片描述个数 = %d, 期望 %d", len(descriptors), wantParts)
	}

	// 键名必须是 additionalPropN（Swagger 占位符风格），不能改成 part1
	for i := 1; i <= wantParts; i++ {
		key := "additionalProp" + strconv.Itoa(i)
		if _, ok := descriptors[key]; !ok {
			t.Errorf("缺少键 %s", key)
			continue
		}
		d := descriptors[key]
		if d.Sha256 == "" || len(d.Sha256) != 64 {
			t.Errorf("%s 的 sha256 = %q, 期望 64 位十六进制", key, d.Sha256)
		}
	}
	// 除最后一片外都应是 64 字节；最后一片是余数
	for i := 1; i < wantParts; i++ {
		if got := descriptors["additionalProp"+strconv.Itoa(i)].Length; got != 64 {
			t.Errorf("第 %d 片长度 = %d, 期望 64", i, got)
		}
	}
	last := descriptors["additionalProp"+strconv.Itoa(wantParts)].Length
	if last <= 0 || last > 64 {
		t.Errorf("最后一片长度 = %d, 应在 (0, 64]", last)
	}
}

func TestPartContentMatchesSHA256(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	// 拿 parts 请求里声明的 sha256，与实际上传的字节比对 ——
	// 这是分片流程最容易出错的地方：算摘要与实际上传用了不同的切片逻辑
	parts := fake.mustFind(t, "multipart/parts")
	var descriptors map[string]PartDescriptor
	if err := json.Unmarshal(parts.Body, &descriptors); err != nil {
		t.Fatal(err)
	}

	all := fake.all()
	var uploaded [][]byte
	for _, r := range all {
		if r.Path == partUpload {
			uploaded = append(uploaded, r.Body)
		}
	}
	wantParts := expectedParts(t, path)
	if len(uploaded) != wantParts {
		t.Fatalf("上传的分片数 = %d, 期望 %d", len(uploaded), wantParts)
	}

	// 拼接后的内容必须与源文件逐字节相同
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, part := range uploaded {
		joined = append(joined, part...)
	}
	if !bytes.Equal(joined, source) {
		t.Errorf("分片拼接结果与源文件不一致：源 %d 字节，拼接 %d 字节",
			len(source), len(joined))
	}

	// 每片的 sha256 必须与声明一致
	for i, part := range uploaded {
		sum := sha256.Sum256(part)
		key := "additionalProp" + strconv.Itoa(i+1)
		if want := descriptors[key].Sha256; hex.EncodeToString(sum[:]) != want {
			t.Errorf("第 %d 片内容与声明的 sha256 不符", i+1)
		}
	}
}

func TestPartUploadForwardsHeadersAndMethod(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	part := fake.mustFind(t, partUpload)
	if part.Method != http.MethodPut {
		t.Errorf("分片上传方法 = %s, 期望 PUT", part.Method)
	}
	// 签名地址是短时效的，必须原样转发华为给的请求头
	if got := part.Header.Get("x-obs-auth"); got != "sig" {
		t.Errorf("应原样转发华为下发的请求头，实际 x-obs-auth = %q", got)
	}
}

func TestComposeSendsETagVerbatim(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	compose := fake.mustFind(t, "multipart/compose")
	var completed map[string]CompletedPart
	if err := json.Unmarshal(compose.Body, &completed); err != nil {
		t.Fatalf("请求体解析失败：%s", compose.Body)
	}
	wantParts := expectedParts(t, path)
	if len(completed) != wantParts {
		t.Fatalf("合并参数个数 = %d, 期望 %d", len(completed), wantParts)
	}
	// ETag 含引号也要原样回传，不要加工。
	// 第 1 片的 ETag 来自假网关的递增计数
	if got := completed["additionalProp1"].ETag; got != `"etag-1"` {
		t.Errorf("etag = %q, 应当原样保留含引号的形式", got)
	}
	if got := completed["additionalProp1"].PartObjectID; got != "po-1" {
		t.Errorf("partObjectId = %q", got)
	}
}

func TestLinkDraftSendsFileNameAndObjectID(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	link := fake.mustFind(t, "app-package-info")
	if link.Method != http.MethodPut {
		t.Errorf("关联草稿方法 = %s, 期望 PUT", link.Method)
	}
	var payload AppPackageInfoReq
	if err := json.Unmarshal(link.Body, &payload); err != nil {
		t.Fatalf("请求体解析失败：%s", link.Body)
	}
	if payload.FileName != "app-HARMONY-1.0.0.app" || payload.ObjectID != "obj-1" {
		t.Errorf("请求体 = %+v", payload)
	}
}

// ---- 送审 ----

func TestSubmitUsesV3Path(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "app-submit")
	// 鸿蒙走 v3、Android 走 v2 —— 路径不同不可混用
	if !strings.Contains(submit.Path, "/v3/") {
		t.Errorf("送审路径 = %s, 鸿蒙必须走 v3", submit.Path)
	}
	if submit.Method != http.MethodPost {
		t.Errorf("送审方法 = %s, 期望 POST", submit.Method)
	}
	// v3 是「appId 走 query + 负载走 body」
	if got := submit.Query.Get("appId"); got != "harmony-app-1" {
		t.Errorf("appId = %q", got)
	}
	if len(submit.Body) == 0 {
		t.Error("v3 送审的负载应当在 body 里")
	}
}

func TestSubmitRetriesOnNotCompiled(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	// 前两次返回「软件包尚未编译完成」
	fake.mu.Lock()
	fake.submitFailTimes = 2
	fake.mu.Unlock()

	path := appPack(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview)); err != nil {
		t.Fatalf("204144660 应当重试后成功：%v", err)
	}

	if got := fake.count("app-submit"); got != 3 {
		t.Errorf("送审调用次数 = %d, 期望 3（前两次失败后重试）", got)
	}
}

func TestSubmitRetryIsBounded(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.mu.Lock()
	fake.submitFailTimes = 100 // 一直失败
	fake.mu.Unlock()
	// 重试上限压到 2 次
	retries := 2
	ch.maxRetries = &retries

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("一直失败应当最终报错")
	}
	// 1 次初始 + 2 次重试 = 3
	if got := fake.count("app-submit"); got != 3 {
		t.Errorf("送审调用次数 = %d, 期望 3（1 次初始 + 2 次重试）", got)
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Code != CodePackageNotCompiled {
		t.Errorf("最终错误应保留原错误码，实际 %v", err)
	}
}

func TestOtherSubmitErrorsAreNotRetried(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.set("/api/publish/v3/app-submit", `{"ret":{"code":204144659,"msg":"other error"}}`)

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	// 只有 204144660 值得重试。其他错误码盲目重试可能造成重复送审
	if got := fake.count("app-submit"); got != 1 {
		t.Errorf("送审调用次数 = %d, 期望 1（非 204144660 不应重试）", got)
	}
}

func TestSubmitFailureIsMarkedAtSubmissionPoint(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.set("/api/publish/v3/app-submit", `<html>502</html>`)

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageSubmitReview))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v, v3 送审是不可撤销的一步", pe.Phase)
	}
	if pe.Retryable() {
		t.Error("送审点的失败绝不能标记为可重试")
	}
}

func TestRemarkOnlySentWhenLengthValid(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	req := testRequest(t, path, channel.StageSubmitReview)
	req.ReleaseParams.UpdateDesc = "太短" // 少于 10 字

	if _, err := ch.Upload(context.Background(), req); err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}

	submit := fake.mustFind(t, "app-submit")
	if bytes.Contains(submit.Body, []byte("remark")) {
		t.Errorf("长度不合规时不应传 remark：%s", submit.Body)
	}
}

// ---- 停留点 ----

func TestStopAtArtifactSkipsLinkAndSubmit(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageUploadArtifact {
		t.Errorf("reached = %v", reached)
	}
	for _, p := range fake.paths() {
		switch {
		case strings.HasSuffix(p, "app-package-info"):
			t.Error("停在仅上传时不应关联草稿")
		case strings.HasSuffix(p, "app-submit"):
			t.Error("停在仅上传时不应送审")
		}
	}
}

func TestStopAtDraftSkipsSubmit(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	path := appPack(t)
	reached, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageCreateDraft))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageCreateDraft {
		t.Errorf("reached = %v", reached)
	}
	if got := fake.count("app-submit"); got != 0 {
		t.Error("停在草稿态时不应送审")
	}
	// 草稿就绪的定义是「已关联」，必须做过
	if got := fake.count("app-package-info"); got != 1 {
		t.Errorf("关联草稿次数 = %d, 期望 1", got)
	}
}

// ---- 失败路径 ----

func TestPartUploadFailureIsReported(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.mu.Lock()
	fake.partFailN = 2 // 第 2 个请求（第 1 片）失败
	fake.mu.Unlock()

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err == nil {
		t.Fatal("分片上传失败应当报错")
	}
	if !strings.Contains(err.Error(), "分片") {
		t.Errorf("应指明是哪个分片失败：%v", err)
	}
}

func TestMissingPartURLIsProtocolError(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	// 只返回 1 片，但文件会被切成更多片。
	//
	// URL 必须是合法的 https —— 用 "PLACEHOLDER" 之类会先失败在地址解析那一步，
	// 就测不到「缺少分片地址」这条路径了
	fake.set(partsPath, `{"ret":{"code":0},"uploadInfoMap":{`+
		`"additionalProp1":{"url":"`+fake.srvURL+partUpload+`","method":"PUT"}}}`)

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err == nil {
		t.Fatal("缺少分片地址应当报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
	// 提示要说明片数与实际返回的条数，否则无从判断是分片算错了还是接口变了
	wantParts := expectedParts(t, path)
	if !strings.Contains(pe.Msg, strconv.Itoa(wantParts)) || !strings.Contains(pe.Msg, "additionalProp2") {
		t.Errorf("应指明缺哪一片、共几片（应有 %d 片）：%s", wantParts, pe.Msg)
	}
}

func TestPartURLMustBeHTTPS(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.set(partsPath, `{"ret":{"code":0},"uploadInfoMap":{`+
		`"additionalProp1":{"url":"http://insecure.example.com/up","method":"PUT"},`+
		`"additionalProp2":{"url":"http://insecure.example.com/up","method":"PUT"},`+
		`"additionalProp3":{"url":"http://insecure.example.com/up","method":"PUT"},`+
		`"additionalProp4":{"url":"http://insecure.example.com/up","method":"PUT"}}}`)

	path := appPack(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, channel.StageUploadArtifact))
	if err == nil {
		t.Fatal("http 分片地址应当被拒绝")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("应说明拒绝原因：%v", err)
	}
}

// TestQueryMarketUsesV3 验证鸿蒙走 v3/app-info 查询状态，并带上审核意见。
//
// 早期版本这里直接报「不支持」，理由是「复用 Android 版 app-info 会查到同名
// Android 应用」—— 那个顾虑本身是对的（HarmonyOS NEXT 应用在 AGC 里是独立记录），
// 但结论下错了：v3 就是鸿蒙专用接口，用 app_id 查到的正是这个鸿蒙应用。
func TestQueryMarketUsesV3(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	fake.set("/api/publish/v3/app-info", `{
		"ret": {"code": 0, "msg": "success"},
		"appInfo": {
			"releaseState": 4,
			"versionCode": 1000012,
			"versionNumber": "1.0.12",
			"onShelfVersionCode": 1000010,
			"onShelfVersionNumber": "1.0.10",
			"releaseTime": "2026-09-28 10:00:00"
		},
		"auditInfo": {"auditOpinion": "正在审核中，请耐心等待"}
	}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.harmony",
		Credentials: channel.NewCredentials(map[string]string{
			ParamClientID: "cid", ParamClientSecret: "sec", ParamAppID: "harmony-app-1",
		}),
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatalf("v3 查询应当可用：%v", err)
	}

	// releaseState=4 是审核中
	if info.ReviewState != channel.ReviewUnderReview {
		t.Errorf("ReviewState = %v, 期望审核中", info.ReviewState)
	}
	// 有在架版本时要优先用它：上层做版本号比对需要的是线上那个版本，
	// 而 versionCode 是正在审核的新版本
	if info.LastVersion == nil {
		t.Fatal("应解析出版本信息")
	}
	if info.LastVersion.Code != 1000010 {
		t.Errorf("LastVersion.Code = %d, 期望 1000010（在架版本，不是审核中的 1000012）",
			info.LastVersion.Code)
	}
	if info.Review == nil || !strings.Contains(info.Review.Opinion, "正在审核中") {
		t.Errorf("应带上审核意见，实际 %+v", info.Review)
	}

	// 确认走的是 v3 路径
	found := false
	for _, p := range fake.paths() {
		if strings.HasSuffix(p, "/v3/app-info") {
			found = true
		}
		if strings.Contains(p, "/v2/app-info") {
			t.Error("不应调用 Android 版的 v2/app-info —— 那查到的是同名 Android 应用")
		}
	}
	if !found {
		t.Errorf("未调用 v3/app-info，实际请求：%v", fake.paths())
	}
}

// 审核意见为空串时不留下空壳，让调用方用 info.Review != nil 判断即可。
func TestQueryMarketEmptyOpinionLeavesNoShell(t *testing.T) {
	ch, fake := newFakeHarmony(t)
	// 官方响应示例里未拒审时就是 "auditOpinion": ""
	fake.set("/api/publish/v3/app-info", `{
		"ret": {"code": 0},
		"appInfo": {"releaseState": 0, "versionCode": 1000000, "versionNumber": "1.0.0"},
		"auditInfo": {"auditOpinion": ""}
	}`)

	info, err := ch.QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.harmony",
		Credentials: channel.NewCredentials(map[string]string{
			ParamClientID: "cid", ParamClientSecret: "sec", ParamAppID: "app-1",
		}),
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewState != channel.ReviewOnline {
		t.Errorf("ReviewState = %v", info.ReviewState)
	}
	if info.Review != nil {
		t.Errorf("意见为空时不应留下空壳结构：%+v", info.Review)
	}
}

// 缺 app_id 时给出凭据错误，而不是笼统的「不支持」。
func TestQueryMarketRequiresAppID(t *testing.T) {
	_, err := New().QueryMarket(context.Background(), channel.MarketQuery{
		ApplicationID: "com.example.harmony",
		Credentials:   channel.NewCredentials(map[string]string{}),
	})
	if err == nil {
		t.Fatal("缺凭据应当报错")
	}
	if !strings.Contains(err.Error(), ParamClientID) {
		t.Errorf("应指出缺哪个参数：%v", err)
	}
}

// ---- 声明 ----

func TestCapabilitiesDeclaration(t *testing.T) {
	caps := New().Capabilities()
	if len(caps.SupportedStages) != 3 {
		t.Fatalf("SupportedStages = %v", caps.SupportedStages)
	}
	if caps.SupportedStages[2] != channel.StageSubmitReview {
		t.Error("阶段列表应以 SubmitReview 结尾")
	}
	if caps.RiskLevel != channel.RiskHigh {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试（204144660 的重试是另一回事：那是明确拒绝）")
	}
	if caps.Withdrawal != channel.WithdrawalAPISupported {
		t.Errorf("Withdrawal = %v", caps.Withdrawal)
	}
}

func TestParamsDeclaration(t *testing.T) {
	params := New().Params()
	required := map[string]bool{}
	for _, p := range params {
		required[p.Name] = p.Required
	}
	for _, name := range []string{ParamClientID, ParamClientSecret, ParamAppID} {
		if !required[name] {
			t.Errorf("%s 应为必填", name)
		}
	}
	// 主体登记信息是可选的：只有部分应用送审时需要
	for _, name := range []string{ParamRegisteredIDType, ParamRegisteredIDNumber} {
		if required[name] {
			t.Errorf("%s 应为可选", name)
		}
	}
}
