// Package logx 提供日志。
//
// # 一条硬约束：绝不写 stdout
//
// MCP 的 stdio transport 用 stdout 独占传输 JSON-RPC 报文，任何一行多余输出都会
// 让宿主解析失败，而且症状是「server 莫名连不上」，极难定位到某个 Println。
//
// 这里不是靠约定，而是靠结构：logger 在构造时就把 handler 绑到 os.Stderr，
// 包内没有任何写 stdout 的路径。Kotlin 版是靠 System.setOut 重定向兜底的，
// 那是补救措施而不是设计。
package logx

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level 与 slog 的级别对应，单独定义是为了 CLI 参数解析不必依赖 slog。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelError
)

func (l Level) slogLevel() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

var (
	mu     sync.Mutex
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	file   *fileSink
)

// L 返回当前 logger。永远写 stderr。
func L() *slog.Logger {
	mu.Lock()
	defer mu.Unlock()
	return logger
}

// Configure 设置级别，并可选地开启文件日志。
//
// dir 为空表示不写文件。文件日志失败不影响 stderr 输出 ——
// 日志目录不可写（权限、磁盘满、只读卷）不应该让程序起不来，
// Kotlin 版正是在 object 初始化块里打开文件，目录不可写就 ExceptionInInitializerError。
func Configure(level Level, dir string) {
	mu.Lock()
	defer mu.Unlock()

	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level.slogLevel()}))

	if file != nil {
		file.close()
		file = nil
	}
	if dir == "" {
		return
	}
	if sink, err := newFileSink(dir); err != nil {
		// 降级为仅 stderr，并把原因说出来
		logger.Warn("日志文件不可用，仅输出到 stderr", "dir", dir, "err", err)
	} else {
		file = sink
	}
}

// Debug / Info / Error 是便捷入口，同时写 stderr 与日志文件。
func Debug(msg string, args ...any) { log(LevelDebug, msg, args...) }
func Info(msg string, args ...any)  { log(LevelInfo, msg, args...) }
func Error(msg string, args ...any) { log(LevelError, msg, args...) }

func log(level Level, msg string, args ...any) {
	mu.Lock()
	lg, sink := logger, file
	mu.Unlock()

	switch level {
	case LevelDebug:
		lg.Debug(msg, args...)
	case LevelError:
		lg.Error(msg, args...)
	default:
		lg.Info(msg, args...)
	}
	if sink != nil {
		sink.write(level, msg, args...)
	}
}

// Flush 把待写内容落盘。进程退出前调用，否则最后几行会丢。
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		file.flush()
	}
}

// ---- 文件输出 ----

type fileSink struct {
	mu  sync.Mutex
	dir string
	day string
	f   *os.File
	w   *bufio.Writer
}

func newFileSink(dir string) (*fileSink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &fileSink{dir: dir}
	if err := s.rotate(time.Now()); err != nil {
		return nil, err
	}
	cleanup(dir)
	return s, nil
}

// rotate 切到当天的日志文件。
//
// 文件名按调用时的日期解析，因此长时间运行的进程跨天后会自动切到新文件 ——
// Kotlin 版在初始化时一次性确定文件名，跨天仍然追加到启动那天的文件里。
func (s *fileSink) rotate(now time.Time) error {
	day := now.Format("2006-01-02")
	if s.f != nil && day == s.day {
		return nil
	}
	if s.f != nil {
		s.w.Flush()
		s.f.Close()
	}
	path := filepath.Join(s.dir, day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	s.day = day
	return nil
}

func (s *fileSink) write(level Level, msg string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotate(time.Now()); err != nil {
		return // 写不了就算了，不能因为日志把主流程拖垮
	}
	var b strings.Builder
	b.WriteString(time.Now().Format("2006-01-02 15:04:05.000"))
	b.WriteString(" ")
	b.WriteString(levelName(level))
	b.WriteString(": ")
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	b.WriteString("\n")
	_, _ = s.w.WriteString(b.String())
	_ = s.w.Flush()
}

func (s *fileSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		_ = s.w.Flush()
	}
}

func (s *fileSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		_ = s.w.Flush()
	}
	if s.f != nil {
		_ = s.f.Close()
	}
	s.f = nil
	s.w = nil
}

func levelName(l Level) string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

const (
	retainDays   = 14
	maxFileBytes = 16 << 20
)

// cleanup 删除过期日志并滚动超限文件。
//
// Kotlin 版按天分文件但从不清理，~/.XiaoZhuan/log/ 会无限增长。
func cleanup(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	deadline := time.Now().AddDate(0, 0, -retainDays)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case info.ModTime().Before(deadline):
			_ = os.Remove(path)
		case info.Size() > maxFileBytes:
			archived := path + ".1"
			_ = os.Remove(archived)
			_ = os.Rename(path, archived)
		}
	}
}

// Redact 脱敏凭据。
//
// 上游项目有两条链路把明文凭据写进日志：一个统一打印返回值的日志辅助函数
// （而 getToken 的返回值就是裸 access_token），以及 debug("保存配置:${apkConfig}")
// （data class 的 toString 会展开全部 clientSecret）。
// 约定：凭据值一律经过本函数才能进日志。
func Redact(value string) string {
	if value == "" {
		return "<empty>"
	}
	if len(value) <= 8 {
		return "***"
	}
	return fmt.Sprintf("%s***%s(len=%d)", value[:4], value[len(value)-2:], len(value))
}
