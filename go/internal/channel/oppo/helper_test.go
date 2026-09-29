package oppo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// newFakeOppo 起一个记录请求的假网关。
//
// baseURL 可注入是这次重写最值得的一处改动：Kotlin 版把 DOMAIN 写成 const val，
// 导致渠道流程完全无法集成测试。这里测试指向 httptest.Server，
// 于是能在没有真实凭据的情况下断言完整的请求形状。
func newFakeOppo(t *testing.T, responses map[string]string) (*httptest.Server, *fakeOppoServer) {
	t.Helper()
	fake := &fakeOppoServer{responses: map[string]string{}}
	for k, v := range responses {
		fake.responses[k] = v
	}
	var mu sync.Mutex

	// 必须用 TLS：OPPO 的 requireHTTPS 会拒绝 http 上传地址，
	// 而假网关要模拟真实的上传地址，所以得是 https
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u := r.URL
		mu.Lock()
		fake.urls = append(fake.urls, u)
		fake.bodies = append(fake.bodies, string(body))
		resp, ok := fake.responses[u.Path]
		mu.Unlock()

		if !ok {
			// 未预设的路径回一个明确的错误，避免测试因为静默成功而失真
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errno":99999,"data":{"message":"unexpected path ` + u.Path + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, fake
}

// bodyFor 取指定路径最后一次请求的 body。
//
// 不能用 bodies[len-1]：提交之后还会轮询 task-state，最后一个请求不再是提交体。
func (f *fakeOppoServer) bodyFor(path string) (string, bool) {
	for i := len(f.urls) - 1; i >= 0; i-- {
		if f.urls[i].Path == path {
			return f.bodies[i], true
		}
	}
	return "", false
}

// 确认 multipart 的 boundary 常量与手工拼装的头部一致。
// 如果 mime/multipart 改变了产出格式，这个测试会失败，
// 提醒我们手工拼装的请求体已经不再兼容。
func TestMultipartBodyIsParseable(t *testing.T) {
	path := testArtifactFile(t)
	reader, length, err := multipartUploadBody(path, "sign-abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != length {
		t.Errorf("预先声明的 ContentLength = %d, 实际读出的字节数 = %d", length, len(data))
	}

	body := string(data)
	// 三个字段都要在
	for _, frag := range []string{
		`name="file"`, `name="type"`, `name="sign"`,
		`filename="app-OPPO-1.0.0.apk"`, "apk", "sign-abc",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("请求体缺少 %q", frag)
		}
	}
	// form-data 而不是 mixed —— 上游注释明确写了 OPPO 不接受 mixed
	if !strings.HasPrefix(body, "--"+boundary+"\r\n") {
		t.Errorf("请求体不是以 boundary 开头：%.60q", body)
	}
	if !strings.HasSuffix(body, "--"+boundary+"--\r\n") {
		t.Errorf("请求体没有正确的结束标记：%.60q", body[len(body)-40:])
	}
}

func TestSignedURLSkipsAPISignInCanonical(t *testing.T) {
	// api_sign 自己不能参与自己的计算。实现里把它放进参数表但值为 nil，
	// 由 Canonicalize 跳过 —— 这条测试确认这个机制真的生效
	api := NewAPI("cid", "secret", nil, "https://example.com")
	raw, err := api.signedURLString("https://example.com/api", map[string]string{"a": "1"}, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	sign := u.Query().Get("api_sign")
	if sign == "" {
		t.Fatal("api_sign 为空")
	}

	// 用同样的参数集重算一遍，验证签名时确实跳过了 api_sign
	params := map[string]*string{"a": ptr("1"), "access_token": ptr("tok"),
		"timestamp": ptr(u.Query().Get("timestamp")), "api_sign": nil}
	if want := Sign("secret", params); want != sign {
		t.Errorf("签名不一致\n  实际: %s\n  重算: %s", sign, want)
	}
	// 反向确认：如果 api_sign 参与了（值为空串而非 nil），签名会不同
	withEmpty := map[string]*string{"a": ptr("1"), "access_token": ptr("tok"),
		"timestamp": ptr(u.Query().Get("timestamp")), "api_sign": ptr("")}
	if Sign("secret", withEmpty) == sign {
		t.Error("api_sign 似乎参与了待签串 —— nil 跳过机制没生效")
	}
}
