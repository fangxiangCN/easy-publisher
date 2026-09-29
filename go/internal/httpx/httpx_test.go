package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

func TestRequireHTTPS(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
		// 报错时必须隐去 query（里面是签名参数），但保留路径（否则看不出是哪个接口）
		wantPathKept  string
		wantQueryGone string
	}{
		{"https://api.example.com/upload/obs?sig=SECRET", false, "", ""},
		{"http://api.example.com/upload/obs?sig=SECRET", true, "/upload/obs", "SECRET"},
		{"ftp://api.example.com/x", true, "", ""},
		{"not a url", true, "", ""},
		{"", true, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			u, err := RequireHTTPS(tc.url, "huawei")
			if tc.wantErr {
				if err == nil {
					t.Fatal("期望拒绝，实际通过")
				}
				var pe *eperr.Error
				if !isProtocol(err, &pe) {
					t.Fatalf("期望 ProtocolMismatch，实际 %v", err)
				}
				if tc.wantPathKept != "" && !strings.Contains(pe.Msg, tc.wantPathKept) {
					t.Errorf("应保留路径便于定位是哪个接口：%s", pe.Msg)
				}
				if tc.wantQueryGone != "" && strings.Contains(pe.Msg, tc.wantQueryGone) {
					t.Errorf("query 里的签名参数不得出现在错误信息里：%s", pe.Msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过，实际拒绝：%v", err)
			}
			if u == nil {
				t.Fatal("返回了 nil URL")
			}
		})
	}
}

func isProtocol(err error, target **eperr.Error) bool {
	pe, ok := err.(*eperr.Error)
	if ok && pe.Kind == eperr.KindProtocolMismatch {
		*target = pe
	}
	return ok && pe.Kind == eperr.KindProtocolMismatch
}

func TestWithoutQueryKeepsPath(t *testing.T) {
	// Kotlin 版这里踩过坑：私有扩展函数取名 redact()，被 OkHttp 的同名成员遮蔽，
	// 实际输出把路径一并抹掉了，连是哪个接口出问题都看不出来
	u, err := RequireHTTPS("https://api.example.com/api/publish/v2/upload-url?a=b&sign=c", "x")
	if err != nil {
		t.Fatal(err)
	}
	got := WithoutQuery(u)
	want := "https://api.example.com/api/publish/v2/upload-url"
	if got != want {
		t.Errorf("WithoutQuery = %q, 期望 %q", got, want)
	}
}

func TestDoReportsStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ret":{"code":403,"msg":"no permission"}}`))
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Do(Client(Default()), req, "huawei")
	if err == nil {
		t.Fatal("期望失败")
	}
	// 上游的 check(response.isSuccessful) 不带 lazyMessage，抛出的是默认的
	// "Check failed."，状态码与响应体全部丢失，导致所有渠道出错时都无法定位
	var pe *eperr.Error
	if !asError(err, &pe) {
		t.Fatalf("类型不对：%T", err)
	}
	if pe.Code != "403" {
		t.Errorf("Code = %q, 期望 403", pe.Code)
	}
	if !strings.Contains(pe.Raw, "no permission") {
		t.Errorf("应带上响应体片段便于定位，实际 Raw = %q", pe.Raw)
	}
}

func TestDoReturnsBodyOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	body, err := Do(Client(Default()), req, "x")
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

func TestUploadReportsProgressAndStopsAtFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	// 1MB，配合 32KB 的读取块会产生多次回调
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Upload(context.Background(), Client(Default()), http.MethodPut, srv.URL,
		map[string]string{"Authorization": "Bearer t"}, path, "x",
		func(fraction float64) { received++ })
	if err != nil {
		t.Fatal(err)
	}
	if received == 0 {
		t.Error("没有收到任何进度回调")
	}
	// 节流生效的标志：1MB / 32KB ≈ 32 次读取，但回调应按 1% 与 200ms 节流，
	// 因此远少于读取次数。上游每 2048 字节回调一次，100MB 会产生约 5 万次
	if received > 110 {
		t.Errorf("回调 %d 次，节流似乎没生效", received)
	}
}

func TestUploadRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Upload(context.Background(), Client(Default()), http.MethodPut,
		"https://example.com", nil, path, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空文件应报「为空」，实际：%v", err)
	}
}

func TestUploadCancelledContextAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, make([]byte, 1<<16), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	// ctx 取消必须让请求失败，而不是把文件传完 ——
	// Kotlin 版的 ProgressBody 要靠 Thread.interrupted() 轮询才能中断
	err := Upload(ctx, Client(Default()), http.MethodPut, "https://example.invalid",
		nil, path, "x", nil)
	if err == nil {
		t.Fatal("已取消的 ctx 应当导致失败")
	}
}

func TestTimeoutsOfSeconds(t *testing.T) {
	got := OfSeconds(300)
	if got.Read != 300*time.Second || got.Write != 300*time.Second {
		t.Errorf("Read/Write = %v / %v", got.Read, got.Write)
	}
	// connect 有上限，避免 --timeout 传一个大值时连接阶段也等很久
	if got.Connect != 60*time.Second {
		t.Errorf("Connect = %v, 期望被限制在 60s", got.Connect)
	}
	if got.Call <= got.Read {
		t.Errorf("Call = %v 应当大于单次读写超时", got.Call)
	}
}

func asError(err error, target **eperr.Error) bool {
	pe, ok := err.(*eperr.Error)
	if ok {
		*target = pe
	}
	return ok
}
