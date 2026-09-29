// Package httpx 提供 HTTP 客户端与请求辅助。
package httpx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// Timeouts 是超时配置。
//
// 上游只有一个硬编码的 60 秒常量，且没有整体调用超时 —— 网络半死状态下，
// 只要单次读写不超时，大文件上传可以长时间悬挂。issue #17 反馈 vivo 渠道
// 60 秒仍然不够，硬编码常量解决不了。
type Timeouts struct {
	// Connect 建立连接的超时
	Connect time.Duration
	// Read 单次读超时
	Read time.Duration
	// Write 单次写超时
	Write time.Duration
	// Call 整体调用超时，覆盖连接 + 读写全过程
	Call time.Duration
}

// Default 是默认超时。
func Default() Timeouts {
	return Timeouts{
		Connect: 30 * time.Second,
		Read:    120 * time.Second,
		Write:   120 * time.Second,
		Call:    60 * time.Minute,
	}
}

// OfSeconds 从「单次请求秒数」推导一组超时，用于 CLI 的 --timeout。
func OfSeconds(seconds int64) Timeouts {
	d := time.Duration(seconds) * time.Second
	connect := d
	if connect > 60*time.Second {
		connect = 60 * time.Second
	}
	call := d * 30
	if call < d {
		call = d
	}
	return Timeouts{Connect: connect, Read: d, Write: d, Call: call}
}

var (
	clientMu sync.Mutex
	clients  = map[Timeouts]*http.Client{}
)

// Client 返回带指定超时的客户端。相同配置复用同一个实例，
// 以共享连接池与传输层。
func Client(t Timeouts) *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c, ok := clients[t]; ok {
		return c
	}
	c := &http.Client{
		Timeout: t.Call,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: t.Connect}).DialContext,
			TLSHandshakeTimeout:   t.Connect,
			ResponseHeaderTimeout: t.Read,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	clients[t] = c
	return c
}

// RequireHTTPS 校验服务端下发的上传地址必须是 https。
//
// 华为、荣耀、鸿蒙、OPPO 的上传地址都来自接口响应。Go 的 http.Client 接受
// http:// —— 若响应被篡改或服务端返回明文地址，整个安装包连同 Authorization
// 头会一起裸奔。
func RequireHTTPS(rawURL, channel string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, eperr.ProtocolError(channel, "渠道返回的上传地址无法解析：%s", truncate(rawURL, 200))
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, eperr.ProtocolError(channel,
			"渠道返回的上传地址不是 https，已拒绝上传：%s", WithoutQuery(u))
	}
	return u, nil
}

// WithoutQuery 隐去 query，避免把签名参数写进日志或错误信息。
//
// 保留路径 —— 只输出 scheme://host 会让人看不出是哪个接口出的问题。
// Kotlin 版这里踩过坑：私有扩展函数取名 redact()，被 OkHttp 的同名成员遮蔽，
// 实际输出把路径一并抹掉了。
func WithoutQuery(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

// ---- 请求执行 ----

// Do 执行请求并返回响应体文本。响应一律关闭，失败时带上状态码与响应体片段。
//
// 上游的 check(response.isSuccessful) 不带 lazyMessage，抛出的是默认的
// "Check failed."，状态码与响应体全部丢失，导致所有渠道出错时都无法定位。
func Do(client *http.Client, req *http.Request, channel string) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", eperr.NetworkError(channel, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if readErr != nil {
		return "", eperr.NetworkError(channel, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &eperr.Error{
			Kind:    eperr.KindChannelRejected,
			Channel: channel,
			Code:    fmt.Sprint(resp.StatusCode),
			Msg:     fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(resp.Status)),
			Raw:     truncate(string(body), maxRawLength),
		}
	}
	return string(body), nil
}

// DoChecked 执行请求，只关心是否成功。失败时把响应体片段带进错误。
func DoChecked(client *http.Client, req *http.Request, channel string) error {
	_, err := Do(client, req, channel)
	return err
}

// Upload 以 PUT/POST 上传文件，带进度回调。
//
// ctx 取消会直接中断在途请求 —— Go 的 http.Client 原生支持这一点。
// Kotlin 版的 ProgressBody 要靠 Thread.interrupted() 轮询才能中断，
// 用户取消后大文件仍会在后台传完。
func Upload(
	ctx context.Context,
	client *http.Client,
	method, rawURL string,
	headers map[string]string,
	path string,
	channel string,
	onProgress func(fraction float64),
) error {
	f, err := os.Open(path)
	if err != nil {
		return eperr.LocalFileError("无法打开待上传文件：%s（%v）", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return eperr.LocalFileError("无法读取文件大小：%s（%v）", path, err)
	}
	if st.Size() == 0 {
		return eperr.LocalFileError("待上传文件为空：%s", path)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL,
		&progressReader{rc: f, total: st.Size(), onProgress: onProgress})
	if err != nil {
		return eperr.LocalFileError("构造上传请求失败：%v", err)
	}
	req.ContentLength = st.Size()
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return DoChecked(client, req, channel)
}

// progressReader 包装文件读取并报告进度。
//
// 按「百分比变化 ≥1% 或间隔 ≥200ms」节流。上游每 2048 字节回调一次，
// 100MB 的包会产生约 5 万次系统调用与 5 万次回调。
type progressReader struct {
	rc         io.ReadCloser
	total      int64
	read       int64
	onProgress func(float64)
	lastPct    int
	lastReport time.Time
}

const reportInterval = 200 * time.Millisecond

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.rc.Read(b)
	if n > 0 {
		p.read += int64(n)
		p.maybeReport(false)
	}
	if err == io.EOF {
		p.maybeReport(true)
	}
	return n, err
}

func (p *progressReader) maybeReport(final bool) {
	if p.onProgress == nil || p.total <= 0 {
		return
	}
	fraction := float64(p.read) / float64(p.total)
	if fraction > 1 {
		fraction = 1
	}
	pct := int(fraction * 100)
	now := time.Now()
	changed := pct != p.lastPct
	if !changed && !final {
		return
	}
	if !final && now.Sub(p.lastReport) < reportInterval && pct != 100 {
		return
	}
	p.lastPct = pct
	p.lastReport = now
	p.onProgress(fraction)
}

const (
	maxBodyBytes = 8 << 20
	maxRawLength = 2000
)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
