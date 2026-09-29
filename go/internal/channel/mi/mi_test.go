package mi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
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

// ---- 黄金向量 ----

const goldenPath = "../../../testdata/golden/signing.json"

// TestRequestDataMatchesGoldenVectors 是小米渠道最重要的验收。
//
// RequestData 是先序列化成字符串、再对同一份字符串算 MD5。JSON 的字段顺序、
// 是否省略 null、数字是否带引号，任何一处不同都会让 MD5 变化，
// 进而让服务端算出的 hash 与我们提交的不一致 —— 表现为笼统的鉴权失败。
//
// 这些 fixture 由 Kotlin 侧生成，并已用 Python 独立复算过。
func TestRequestDataMatchesGoldenVectors(t *testing.T) {
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读取黄金向量失败（%v）。先在仓库根目录运行：\n"+
			"  ./gradlew :core:test --tests \"*GoldenVectorTest*\" -Dgolden.update=true --rerun-tasks", err)
	}
	var golden struct {
		MiQuery []struct {
			Name            string `json:"name"`
			Account         string `json:"account"`
			PackageName     string `json:"packageName"`
			RequestDataJSON string `json:"requestDataJson"`
			RequestDataMD5  string `json:"requestDataMd5"`
		} `json:"miQueryRequestData"`
		MiPush []struct {
			Name            string `json:"name"`
			Account         string `json:"account"`
			AppName         string `json:"appName"`
			PackageName     string `json:"packageName"`
			UpdateDesc      string `json:"updateDesc"`
			OnlineTime      int64  `json:"onlineTime"`
			RequestDataJSON string `json:"requestDataJson"`
			RequestDataMD5  string `json:"requestDataMd5"`
		} `json:"miPushRequestData"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.MiQuery) == 0 || len(golden.MiPush) == 0 {
		t.Fatal("黄金向量里缺少小米用例")
	}

	for _, v := range golden.MiQuery {
		t.Run("query/"+v.Name, func(t *testing.T) {
			got, err := BuildQueryRequestData(v.Account, v.PackageName)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, got, v.RequestDataJSON, v.RequestDataMD5)
		})
	}

	for _, v := range golden.MiPush {
		t.Run("push/"+v.Name, func(t *testing.T) {
			got, err := BuildPushRequestData(v.Account, PackageInfo{
				AppName: v.AppName, PackageName: v.PackageName,
			}, v.UpdateDesc, v.OnlineTime)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, got, v.RequestDataJSON, v.RequestDataMD5)
		})
	}
}

func assertGolden(t *testing.T, gotJSON, wantJSON, wantMD5 string) {
	t.Helper()
	if gotJSON != wantJSON {
		t.Errorf("RequestData 字节不一致\n  Go:     %s\n  golden: %s", gotJSON, wantJSON)
	}
	if md5 := MD5Hex(gotJSON); md5 != wantMD5 {
		t.Errorf("MD5 不一致\n  Go:     %s\n  golden: %s", md5, wantMD5)
	}
}

// TestEmptyStringsAreNotOmitted 覆盖一个容易踩的跨语言差异。
//
// Moshi 只省略 **null**，空字符串仍会输出；而 Go 的 omitempty 连零值也省。
// 若给 updateDesc 之类加了 omitempty，描述为空时两边 JSON 就不同，
// MD5 对不上，表现为笼统的鉴权失败 —— 从错误信息完全看不出原因。
func TestEmptyStringsAreNotOmitted(t *testing.T) {
	got, err := BuildPushRequestData("a@b.c", PackageInfo{
		AppName: "", PackageName: "p",
	}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"appName":""`, `"updateDesc":""`} {
		if !strings.Contains(got, field) {
			t.Errorf("空字符串必须保留（Moshi 只省略 null），缺少 %s：%s", field, got)
		}
	}
	// onlineTime 是例外：它在语义上就是可选，0 表示不带该字段
	if strings.Contains(got, "onlineTime") {
		t.Errorf("onlineTime 为 0 时应省略：%s", got)
	}

	query, err := BuildQueryRequestData("a@b.c", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `"packageName":""`) {
		t.Errorf("空包名必须保留：%s", query)
	}
}

func TestPushRequestFieldOrder(t *testing.T) {
	// 字段顺序影响 MD5，必须与 Kotlin 版一致
	got, err := BuildPushRequestData("a@b.c", PackageInfo{
		AppName: "n", PackageName: "p",
	}, "desc", 1767225600000)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"userName":"a@b.c","synchroType":1,"appInfo":{"appName":"n",` +
		`"packageName":"p","updateDesc":"desc","onlineTime":1767225600000}}`
	if got != want {
		t.Errorf("字段顺序或结构不符\n  Go:     %s\n  期望: %s", got, want)
	}
}

// ---- RSA 加密 ----
//
// PKCS#1 v1.5 填充含随机数，**同一明文每次密文都不同**。断言密文相等的测试会
// 永远失败，然后让人怀疑实现错了。因此改为验证三件确定性的事：
//   - 明文（SIG 的 JSON）字节
//   - 密文长度关系 ceil(n/117)*128
//   - 加解密回环

// testCert 生成一个 1024 位的自签证书并返回 PEM 文本与私钥。
//
// 用 1024 位而非 2048：小米下发的就是 1024 位，分组大小（117/128）
// 直接由密钥长度决定，用别的长度测不出真实行为。
func testCert(t *testing.T) (certPEM string, key *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, KeySize)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mi-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key
}

func TestEncryptRoundTrip(t *testing.T) {
	cert, key := testCert(t)

	// 覆盖分组边界：不足一组、恰好一组、跨多组
	plaintexts := []string{
		"",
		"a",
		strings.Repeat("x", EncryptGroupSize-1),
		strings.Repeat("x", EncryptGroupSize),
		strings.Repeat("x", EncryptGroupSize+1),
		strings.Repeat("x", EncryptGroupSize*3+50),
		`{"password":"p","sig":[{"name":"RequestData","hash":"abc"}]}`,
	}

	for _, plain := range plaintexts {
		t.Run(fmt.Sprintf("len=%d", len(plain)), func(t *testing.T) {
			cipherHex, err := Encrypt(plain, cert)
			if err != nil {
				t.Fatal(err)
			}

			// 长度关系：每 117 字节明文产生 128 字节密文，十六进制翻倍
			wantGroups := EncryptGroupCount(len(plain))
			if got := len(cipherHex) / 2; got != wantGroups*GroupSize {
				t.Errorf("密文长度 = %d 字节, 期望 %d（%d 组 × %d）",
					got, wantGroups*GroupSize, wantGroups, GroupSize)
			}
			if cipherHex != strings.ToLower(cipherHex) {
				t.Error("密文必须是小写十六进制")
			}

			// 回环：解密后必须还原出原始明文
			raw, err := hexDecode(cipherHex)
			if err != nil {
				t.Fatal(err)
			}
			var restored []byte
			for off := 0; off < len(raw); off += GroupSize {
				chunk, err := rsa.DecryptPKCS1v15(rand.Reader, key, raw[off:off+GroupSize])
				if err != nil {
					t.Fatalf("第 %d 组解密失败：%v", off/GroupSize+1, err)
				}
				restored = append(restored, chunk...)
			}
			if string(restored) != plain {
				t.Errorf("回环失败\n  原文: %q\n  还原: %q", plain, string(restored))
			}
		})
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	// 这条测试记录一个事实：PKCS#1 v1.5 填充含随机数，同一明文两次加密结果不同。
	//
	// 它不是「测试实现的随机性」，而是**防止后来人写出断言密文相等的测试** ——
	// 那种测试会永远失败，然后让人以为是实现错了。
	cert, _ := testCert(t)
	plain := `{"password":"p","sig":[]}`
	a, err := Encrypt(plain, cert)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(plain, cert)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("同一明文两次加密结果相同，说明填充没有用随机数 —— " +
			"若真如此，服务端的解密行为也可能与我们预期不符")
	}
	if len(a) != len(b) {
		t.Errorf("两次密文长度不同：%d vs %d", len(a), len(b))
	}
}

func TestEncryptUsesCorrectGroupBoundary(t *testing.T) {
	// 117 是 PKCS#1 v1.5 在 1024 位密钥下的明文上限。
	// 若误用 128 作分组，rsa.EncryptPKCS1v15 会返回 ErrMessageTooLong；
	// 若用小了，密文长度会多出一组
	cert, _ := testCert(t)

	at117, err := Encrypt(strings.Repeat("x", EncryptGroupSize), cert)
	if err != nil {
		t.Fatalf("恰好 117 字节应当一组装下：%v", err)
	}
	if got := len(at117) / 2; got != GroupSize {
		t.Errorf("117 字节明文产生 %d 字节密文, 期望 %d", got, GroupSize)
	}

	at118, err := Encrypt(strings.Repeat("x", EncryptGroupSize+1), cert)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(at118) / 2; got != GroupSize*2 {
		t.Errorf("118 字节明文产生 %d 字节密文, 期望 %d（两组）", got, GroupSize*2)
	}
}

func TestParsePublicKeyAcceptsPEMAndDER(t *testing.T) {
	certPEM, _ := testCert(t)
	if _, err := parsePublicKey(certPEM); err != nil {
		t.Errorf("PEM 应当被接受：%v", err)
	}

	// Java 的 CertificateFactory("X.509") 同时接受 DER 与 PEM，Go 侧保持一致
	block, _ := pem.Decode([]byte(certPEM))
	if _, err := parsePublicKey(string(block.Bytes)); err != nil {
		t.Errorf("DER 应当被接受：%v", err)
	}
}

func TestParsePublicKeyErrors(t *testing.T) {
	if _, err := parsePublicKey(""); err == nil {
		t.Error("空证书应当报错")
	}
	if _, err := parsePublicKey("   "); err == nil {
		t.Error("纯空白应当报错")
	}

	_, err := parsePublicKey("这不是证书")
	if err == nil {
		t.Fatal("非法内容应当报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindCredential {
		t.Errorf("证书无效属于凭据问题，实际 %v", err)
	}
	// 证书无效是用户填错了，提示要指出该填什么
	if !strings.Contains(pe.Msg, ".cer") {
		t.Errorf("应说明该填什么文件：%s", pe.Msg)
	}
	// 异常信息里绝不能带证书内容
	if strings.Contains(pe.Msg, "这不是证书") {
		t.Errorf("错误信息不得回显证书内容：%s", pe.Msg)
	}
}

// ---- 全流程 ----

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
	// Form 是解析后的表单或 multipart 字段
	Form map[string]string
	// APKFileName 是 multipart 里 apk part 的文件名
	APKFileName string
	APKSize     int64
}

type fakeMi struct {
	mu        sync.Mutex
	responses map[string]string
	requests  []recorded
}

func (f *fakeMi) set(path, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = resp
}

func (f *fakeMi) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeMi) paths() []string {
	out := []string{}
	for _, r := range f.all() {
		out = append(out, r.Path)
	}
	return out
}

func (f *fakeMi) mustFind(t *testing.T, suffix string) recorded {
	t.Helper()
	for _, r := range f.all() {
		if strings.HasSuffix(r.Path, suffix) {
			return r
		}
	}
	t.Fatalf("没找到路径以 %q 结尾的请求，实际：%v", suffix, f.paths())
	return recorded{}
}

var miDefaults = map[string]string{
	PathQuery: `{"result":0,"message":"ok","updateVersion":true,` +
		`"packageInfo":{"appName":"示例应用","versionName":"1.0.0",` +
		`"versionCode":1000,"packageName":"com.example.app"}}`,
	PathPush: `{"result":0,"message":"ok"}`,
}

func newFakeMi(t *testing.T) (*Channel, *fakeMi, string, *rsa.PrivateKey) {
	t.Helper()
	certPEM, key := testCert(t)
	fake := &fakeMi{responses: map[string]string{}}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		rec := recorded{
			Method: r.Method, Path: r.URL.Path,
			Header: r.Header.Clone(), Body: body,
			Form: map[string]string{},
		}
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/") {
			_, params, err := mime.ParseMediaType(ct)
			if err == nil {
				mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
				for {
					part, err := mr.NextPart()
					if err != nil {
						break
					}
					data, _ := io.ReadAll(part)
					rec.Form[part.FormName()] = string(data)
					if part.FormName() == APKPartName {
						rec.APKFileName = part.FileName()
						rec.APKSize = int64(len(data))
					}
				}
			}
		} else {
			for _, kv := range strings.Split(string(body), "&") {
				if i := strings.IndexByte(kv, '='); i > 0 {
					rec.Form[urlDecode(kv[:i])] = urlDecode(kv[i+1:])
				}
			}
		}

		fake.mu.Lock()
		fake.requests = append(fake.requests, rec)
		resp, ok := fake.responses[r.URL.Path]
		fake.mu.Unlock()
		if !ok {
			resp, ok = miDefaults[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"result":404,"message":"unexpected"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)

	ch := NewWithBaseURL(srv.URL)
	ch.newClient = func(httpx.Timeouts) *http.Client { return srv.Client() }
	return ch, fake, certPEM, key
}

func testArtifactFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-MI-1.0.0.apk")
	if err := os.WriteFile(path, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRequest(t *testing.T, path, cert string) channel.UploadRequest {
	t.Helper()
	return channel.UploadRequest{
		ArtifactFile: path,
		ArtifactInfo: artifact.Info{
			Path: path, ApplicationID: "com.example.app",
			VersionCode: 1020, VersionName: "1.2.0", Kind: artifact.KindAPK,
		},
		Credentials: channel.NewCredentials(map[string]string{
			ParamAccount:    "test@example.com",
			ParamPublicKey:  cert,
			ParamPrivateKey: "api-password",
		}),
		ReleaseParams: channel.ReleaseParams{UpdateDesc: "修复若干问题"},
		Timeouts:      httpx.Default(),
		StopAfter:     channel.StageSubmitReview,
	}
}

func TestUploadFullFlow(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	path := testArtifactFile(t)

	reached, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err != nil {
		t.Fatalf("Upload 失败: %v", err)
	}
	if reached != channel.StageSubmitReview {
		t.Errorf("reached = %v", reached)
	}

	// 两步：先 query（拿 appName），再 push
	want := []string{PathQuery, PathPush}
	got := fake.paths()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("调用顺序 = %v, 期望 %v", got, want)
	}
}

func TestQueryUsesFormEncoding(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, cert)); err != nil {
		t.Fatal(err)
	}

	query := fake.mustFind(t, PathQuery)
	if query.Method != http.MethodPost {
		t.Errorf("方法 = %s, 期望 POST", query.Method)
	}
	if ct := query.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	if query.Form[RequestDataField] == "" || query.Form[SIGField] == "" {
		t.Errorf("表单应含 RequestData 与 SIG：%v", query.Form)
	}
	// RequestData 应当是纯 JSON
	var parsed QueryRequest
	if err := json.Unmarshal([]byte(query.Form[RequestDataField]), &parsed); err != nil {
		t.Errorf("RequestData 不是合法 JSON：%s", query.Form[RequestDataField])
	}
}

func TestPushMultipartStructure(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, cert)); err != nil {
		t.Fatal(err)
	}

	push := fake.mustFind(t, PathPush)
	if push.Method != http.MethodPost {
		t.Errorf("方法 = %s, 期望 POST", push.Method)
	}
	if ct := push.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("Content-Type = %q", ct)
	}
	// 三个字段都要在
	for _, field := range []string{APKPartName, RequestDataField, SIGField} {
		if _, ok := push.Form[field]; !ok {
			t.Errorf("multipart 缺少字段 %s", field)
		}
	}
	// apk part 的文件名是**空串**：小米服务端按 part 名取文件，
	// 填入真实文件名未经验证。看着像 bug，但必须保持原样
	if push.APKFileName != "" {
		t.Errorf("apk part 的文件名应为空串，实际 %q", push.APKFileName)
	}
	if push.APKSize != 2048 {
		t.Errorf("上传的 apk 字节数 = %d, 期望 2048", push.APKSize)
	}
}

func TestSigPayloadStructure(t *testing.T) {
	ch, fake, cert, key := newFakeMi(t)
	path := testArtifactFile(t)
	if _, err := ch.Upload(context.Background(), testRequest(t, path, cert)); err != nil {
		t.Fatal(err)
	}

	// 解密 SIG，检查明文结构
	push := fake.mustFind(t, PathPush)
	plain := decryptSig(t, push.Form[SIGField], key)

	var payload SigPayload
	if err := json.Unmarshal([]byte(plain), &payload); err != nil {
		t.Fatalf("SIG 明文不是合法 JSON：%s", plain)
	}
	if payload.Password != "api-password" {
		t.Errorf("password = %q", payload.Password)
	}
	if len(payload.Sig) != 2 {
		t.Fatalf("sig 条目数 = %d, 期望 2（RequestData 与 apk）", len(payload.Sig))
	}
	if payload.Sig[0].Name != RequestDataField || payload.Sig[1].Name != APKPartName {
		t.Errorf("sig 条目顺序或名称不对：%+v", payload.Sig)
	}

	// RequestData 的 hash 必须与表单里实际发送的那份字符串一致 ——
	// 「序列化一次」是这里的核心约束
	if want := MD5Hex(push.Form[RequestDataField]); payload.Sig[0].Hash != want {
		t.Errorf("RequestData 的 hash 与实际发送的字符串不匹配\n  声明: %s\n  实算: %s",
			payload.Sig[0].Hash, want)
	}
	// apk 的 hash 必须是文件内容的 MD5
	apkData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := MD5Hex(string(apkData)); payload.Sig[1].Hash != want {
		t.Errorf("apk 的 hash 与文件内容不匹配\n  声明: %s\n  实算: %s",
			payload.Sig[1].Hash, want)
	}
}

func TestSigPlaintextIsDeterministic(t *testing.T) {
	// 密文不可比对，但**明文可以** —— 这是跨语言验证的关键。
	// 用同一份输入构造两次 SIG 明文，字节必须相同
	items := []SigItem{
		{Name: RequestDataField, Hash: MD5Hex(`{"a":1}`)},
		{Name: APKPartName, Hash: MD5Hex("apk-content")},
	}
	a, err := BuildSig("pwd", items)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildSig("pwd", items)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("SIG 明文应当确定性\n  %s\n  %s", a, b)
	}
	want := `{"password":"pwd","sig":[{"name":"RequestData","hash":"` + MD5Hex(`{"a":1}`) +
		`"},{"name":"apk","hash":"` + MD5Hex("apk-content") + `"}]}`
	if a != want {
		t.Errorf("SIG 结构不符\n  Go:   %s\n  期望: %s", a, want)
	}
}

// ---- 失败路径 ----

func TestRejectsDraftStage(t *testing.T) {
	// dev/push 是原子的，无处可停。必须显式报错，不能默默走到送审
	ch, _, cert, _ := newFakeMi(t)
	path := testArtifactFile(t)
	req := testRequest(t, path, cert)
	req.StopAfter = channel.StageCreateDraft

	_, err := ch.Upload(context.Background(), req)
	if err == nil {
		t.Fatal("小米不支持停在草稿态")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "送审") {
		t.Errorf("应告知该渠道只能停在送审：%v", err)
	}
}

func TestRejectsUploadArtifactStage(t *testing.T) {
	ch, _, cert, _ := newFakeMi(t)
	path := testArtifactFile(t)
	req := testRequest(t, path, cert)
	req.StopAfter = channel.StageUploadArtifact

	if _, err := ch.Upload(context.Background(), req); err == nil {
		t.Fatal("小米不支持只上传安装包")
	}
}

func TestMissingPackageInfoGivesActionableError(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	fake.set(PathQuery, `{"result":0,"message":"ok"}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err == nil {
		t.Fatal("缺少 packageInfo 应当报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindProtocolMismatch {
		t.Errorf("期望 ProtocolMismatch，实际 %v", err)
	}
	// 用户看到的不该只是一句「缺字段」，要指向可执行的动作
	if !strings.Contains(pe.Msg, "小米开放平台") {
		t.Errorf("应给出下一步动作：%s", pe.Msg)
	}
}

func TestMissingResultIsProtocolError(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	// 上游用 get("result").asInt 手读，字段缺失时直接 NPE
	fake.set(PathQuery, `{"message":"ok"}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err == nil {
		t.Fatal("缺少 result 应当报错而不是当成成功")
	}
	if !strings.Contains(err.Error(), "result") {
		t.Errorf("应说明缺的是 result：%v", err)
	}
}

func TestChannelRejectedSurfacesCode(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	fake.set(PathPush, `{"result":10001,"message":"签名验证失败"}`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindChannelRejected {
		t.Fatalf("期望 ChannelRejected，实际 %v", err)
	}
	if pe.Code != "10001" {
		t.Errorf("Code = %q", pe.Code)
	}
	if !strings.Contains(pe.Msg, "code=10001") {
		t.Errorf("message 应带上错误码：%s", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "签名验证失败") {
		t.Errorf("应保留渠道的原始描述：%s", pe.Msg)
	}
}

func TestPushFailureIsNotRetryable(t *testing.T) {
	// dev/push 把上传与送审合并在一次请求里，整个调用都在不可撤销区间。
	// 上传途中失败其实大概率没被受理，但客户端无法区分「上传中断」与
	// 「已受理但响应丢失」，而两者代价不对称
	ch, fake, cert, _ := newFakeMi(t)
	fake.set(PathPush, `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		t.Fatalf("类型不对: %T", err)
	}
	if pe.Phase != eperr.PhaseAtOrAfterSubmission {
		t.Errorf("Phase = %v", pe.Phase)
	}
	if pe.Retryable() {
		t.Error("小米的 push 失败绝不能标记为可重试")
	}
	if !strings.Contains(pe.Msg, "确认") {
		t.Errorf("提示应引导先到后台确认：%s", pe.Msg)
	}
}

func TestQueryFailureStaysRetryable(t *testing.T) {
	ch, fake, cert, _ := newFakeMi(t)
	fake.set(PathQuery, `<html>502</html>`)

	path := testArtifactFile(t)
	_, err := ch.Upload(context.Background(), testRequest(t, path, cert))
	if err == nil {
		t.Fatal("期望失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Phase != eperr.PhasePreSubmission {
		t.Errorf("查应用信息在送审之前，失败应保持可重试，实际 %v", err)
	}
}

func TestBadCertificateFailsBeforeNetwork(t *testing.T) {
	ch, fake, _, _ := newFakeMi(t)
	path := testArtifactFile(t)
	const badCert = "这不是证书"
	req := testRequest(t, path, badCert)
	req.Credentials = channel.NewCredentials(map[string]string{
		ParamAccount: "a@b.c", ParamPublicKey: badCert, ParamPrivateKey: "p",
	})

	_, err := ch.Upload(context.Background(), req)
	if err == nil {
		t.Fatal("证书无效应当报错")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindCredential {
		t.Errorf("证书无效属于凭据问题，实际 %v", err)
	}
	// 证书在第一步就要用到（加密 SIG），因此不该有任何成功的业务请求
	for _, r := range fake.all() {
		if r.Path == PathPush {
			t.Error("证书无效时不应发出 push 请求")
		}
	}
}

// ---- 状态映射 ----

func TestToMarketInfo(t *testing.T) {
	cases := []struct {
		name          string
		updateVersion *bool
		want          channel.ReviewState
	}{
		{"true 表示可更新", boolPtr(true), channel.ReviewOnline},
		{"false 表示有版本在审核中", boolPtr(false), channel.ReviewUnderReview},
		// 缺失时不能猜：猜 true 会让上层误以为可以提交，猜 false 又会拦住正常发布
		{"缺失归为未知", nil, channel.ReviewUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := AppInfoResp{UpdateVersion: tc.updateVersion}
			got := resp.ToMarketInfo()
			if got.ReviewState != tc.want {
				t.Errorf("ReviewState = %v, 期望 %v", got.ReviewState, tc.want)
			}
			if !strings.Contains(got.RawState, "updateVersion") {
				t.Errorf("RawState = %q, 应保留原始值便于排查", got.RawState)
			}
		})
	}
}

func TestVersionMissingDoesNotFabricate(t *testing.T) {
	// 小米在「审核中」状态下不返回正在审核的版本号，这属于正常情况。
	// 伪造 0 会让版本号比对得出错误结论
	resp := AppInfoResp{
		UpdateVersion: boolPtr(false),
		PackageInfo:   &PackageInfo{PackageName: "com.example.app"},
	}
	if got := resp.ToMarketInfo(); got.LastVersion != nil {
		t.Errorf("LastVersion = %v, 缺少 versionCode 时应为 nil", got.LastVersion)
	}

	// 有版本号时要正常带出来
	code := int64(1020)
	resp.PackageInfo.VersionCode = &code
	resp.PackageInfo.VersionName = "1.2.0"
	got := resp.ToMarketInfo()
	if got.LastVersion == nil || got.LastVersion.Code != 1020 {
		t.Errorf("LastVersion = %v", got.LastVersion)
	}
}

// ---- 声明 ----

func TestCapabilitiesDeclaration(t *testing.T) {
	caps := New().Capabilities()
	// dev/push 原子，只有送审一个阶段
	if len(caps.SupportedStages) != 1 || caps.SupportedStages[0] != channel.StageSubmitReview {
		t.Errorf("SupportedStages = %v, 小米只应支持送审", caps.SupportedStages)
	}
	if caps.RiskLevel != channel.RiskCritical {
		t.Errorf("RiskLevel = %v", caps.RiskLevel)
	}
	if caps.AutomaticRetryAfterSubmission {
		t.Error("送审后不允许自动重试")
	}
	// 小米是六个渠道里唯一完全未验证过的
	if caps.Evidence != channel.EvidenceCodeObservation {
		t.Errorf("Evidence = %v, 小米尚未用真实凭据验证过", caps.Evidence)
	}
	if caps.VerifiedScope != "" {
		t.Errorf("未实测的渠道不应有验证范围，实际 %q", caps.VerifiedScope)
	}
}

func TestParamsDeclaration(t *testing.T) {
	params := New().Params()
	if len(params) != 3 {
		t.Fatalf("参数个数 = %d", len(params))
	}
	byName := map[string]channel.ChannelParam{}
	for _, p := range params {
		byName[p.Name] = p
	}
	// 公钥证书是文件型参数
	cert, ok := byName[ParamPublicKey]
	if !ok {
		t.Fatal("缺少 publicKey 参数")
	}
	if cert.Type != channel.ParamTextFile || cert.FileExtension != "cer" {
		t.Errorf("publicKey 应为 .cer 文件型参数，实际 %+v", cert)
	}
}

// ---- 辅助 ----

func boolPtr(v bool) *bool { return &v }

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

// decryptSig 按分组解密 SIG 密文，还原明文。
func decryptSig(t *testing.T, cipherHex string, key *rsa.PrivateKey) string {
	t.Helper()
	raw, err := hexDecode(cipherHex)
	if err != nil {
		t.Fatalf("SIG 不是合法十六进制：%v", err)
	}
	if len(raw)%GroupSize != 0 {
		t.Fatalf("SIG 密文长度 %d 不是 %d 的整数倍", len(raw), GroupSize)
	}
	var out []byte
	for off := 0; off < len(raw); off += GroupSize {
		chunk, err := rsa.DecryptPKCS1v15(rand.Reader, key, raw[off:off+GroupSize])
		if err != nil {
			t.Fatalf("解密第 %d 组失败：%v", off/GroupSize+1, err)
		}
		out = append(out, chunk...)
	}
	return string(out)
}

// urlDecode 解表单字段。给假网关解析请求体用。
func urlDecode(s string) string {
	out, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return out
}
