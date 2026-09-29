// Package config 管理应用配置与渠道凭据。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
)

// Param 是一个渠道参数的键值对。
type Param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// String 脱敏输出。
//
// 上游项目的 data class 默认 toString() 会展开 Value，配合
// debug("保存配置:${apkConfig}") 把全部 clientSecret 与私钥打进日志。
// Go 里对应的是 %+v，因此凡是可能被打印的类型都要自己实现 Stringer。
func (p Param) String() string {
	if IsSensitiveKey(p.Name) {
		return fmt.Sprintf("Param(%s=%s)", p.Name, logx.Redact(p.Value))
	}
	return fmt.Sprintf("Param(%s=%s)", p.Name, p.Value)
}

// ChannelConfig 是一个渠道的配置。
type ChannelConfig struct {
	Name    string  `json:"name"`
	Enabled bool    `json:"enabled"`
	Params  []Param `json:"params"`
}

func (c ChannelConfig) String() string {
	names := make([]string, 0, len(c.Params))
	for _, p := range c.Params {
		names = append(names, p.Name)
	}
	return fmt.Sprintf("ChannelConfig(%s, enabled=%t, params=%v)", c.Name, c.Enabled, names)
}

// Param 按名字取参数，不区分大小写。
func (c ChannelConfig) Param(name string) (Param, bool) {
	for _, p := range c.Params {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Param{}, false
}

// Value 取参数值，空值视为未配置。
func (c ChannelConfig) Value(name string) (string, bool) {
	p, ok := c.Param(name)
	if !ok || strings.TrimSpace(p.Value) == "" {
		return "", false
	}
	return p.Value, true
}

// WithParam 返回设置了指定参数的副本。
func (c ChannelConfig) WithParam(name, value string) ChannelConfig {
	out := ChannelConfig{Name: c.Name, Enabled: c.Enabled}
	replaced := false
	for _, p := range c.Params {
		if strings.EqualFold(p.Name, name) {
			out.Params = append(out.Params, Param{Name: name, Value: value})
			replaced = true
		} else {
			out.Params = append(out.Params, p)
		}
	}
	if !replaced {
		out.Params = append(out.Params, Param{Name: name, Value: value})
	}
	return out
}

// AppConfig 是一个待发布应用的配置。
//
// 字段名与 JSON tag 与 Kotlin 版逐字一致，两边的配置文件可以互换。
type AppConfig struct {
	Name            string          `json:"name"`
	ApplicationID   string          `json:"applicationId"`
	CreateTime      int64           `json:"createTime"`
	Channels        []ChannelConfig `json:"channels"`
	MultiChannelApk bool            `json:"multiChannelApk"`

	// ExpectedLabel 是商店页展示的应用名，用于上架前比对 APK 内的
	// android:label。两者不一致会被渠道驳回 —— 华为实测报
	// "AppName is not same as it in apk package"。
	//
	// 可选：未配置时 checklist 只提示 APK 内的实际名称，不做一致性判定。
	// 新增字段对老配置文件无害（零值即未配置）。
	ExpectedLabel string `json:"expectedLabel,omitempty"`
}

// String 不输出参数值，避免凭据进入日志。
func (a AppConfig) String() string {
	names := make([]string, 0, len(a.Channels))
	for _, c := range a.Channels {
		names = append(names, c.Name)
	}
	return fmt.Sprintf("AppConfig(%s, name=%s, channels=%v)", a.ApplicationID, a.Name, names)
}

// Channel 按 id 取渠道配置，不区分大小写。
func (a AppConfig) Channel(id string) (ChannelConfig, bool) {
	for _, c := range a.Channels {
		if strings.EqualFold(c.Name, id) {
			return c, true
		}
	}
	return ChannelConfig{}, false
}

func (a AppConfig) IsChannelEnabled(id string) bool {
	c, ok := a.Channel(id)
	return ok && c.Enabled
}

// EnabledChannels 返回启用的渠道配置。
func (a AppConfig) EnabledChannels() []ChannelConfig {
	var out []ChannelConfig
	for _, c := range a.Channels {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// WithChannel 返回替换/新增指定渠道后的副本。
func (a AppConfig) WithChannel(ch ChannelConfig) AppConfig {
	out := a
	out.Channels = make([]ChannelConfig, 0, len(a.Channels)+1)
	for _, c := range a.Channels {
		if !strings.EqualFold(c.Name, ch.Name) {
			out.Channels = append(out.Channels, c)
		}
	}
	out.Channels = append(out.Channels, ch)
	return out
}

// ---- 存储 ----

const fileSuffix = ".json"

// Store 持久化应用配置。
//
// 相对上游实现修正了三处：
//
//  1. 原子写。上游直接 file.writeText(json)，非原子。并发写同一文件会产生截断的
//     JSON，而读取失败只 printStackTrace 并静默返回 null，用户的全部渠道凭据会
//     「凭空消失」且无迹可查。这里写临时文件后 rename 替换。
//  2. 权限 0600。上游继承 umask（通常 644），同机其他用户可读凭据。
//  3. 真正删除。上游的 removeConfig 只是 rename 成 .bak 并不删除内容，
//     而列表扫描只认 .json —— 用户以为删掉了凭据，明文其实长期留在磁盘上。
//
// 另外上游的 saveApkConfig 流程是先 removeConfig（把原文件改名）再 saveConfig，
// 中间失败会导致配置彻底丢失且无回滚。这里的 Save 是单次原子替换，不存在中间态。
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore 创建存储。dir 为空时用默认位置 ~/.easy-publisher/apps。
func NewStore(dir string) *Store {
	if dir == "" {
		dir = filepath.Join(HomeDir(), "apps")
	}
	return &Store{dir: dir}
}

// HomeDir 返回配置根目录。
//
// 可用环境变量 EP_HOME 覆盖，便于测试与 CI 隔离。
func HomeDir() string {
	if override := strings.TrimSpace(os.Getenv("EP_HOME")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// 拿不到 home 时退回当前目录下的隐藏目录，而不是 panic ——
		// 只读操作（如 channel list）不该因为无法确定 home 就完全不可用
		return ".easy-publisher"
	}
	return filepath.Join(home, ".easy-publisher")
}

func (s *Store) pathFor(applicationID string) (string, error) {
	id, err := validateID(applicationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+fileSuffix), nil
}

// List 返回全部配置，按文件名排序。
func (s *Store) List() ([]AppConfig, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, eperr.ConfigurationError("读取配置目录失败：%v", err)
	}
	var out []AppConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		if cfg, err := s.read(filepath.Join(s.dir, e.Name())); err == nil {
			out = append(out, cfg)
		}
	}
	return out, nil
}

// Get 读取一个配置，不存在时返回 ok=false。
func (s *Store) Get(applicationID string) (AppConfig, bool, error) {
	path, err := s.pathFor(applicationID)
	if err != nil {
		return AppConfig{}, false, err
	}
	cfg, err := s.read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return AppConfig{}, false, nil
		}
		return AppConfig{}, false, err
	}
	return cfg, true, nil
}

// Require 读取一个配置，不存在时报带下一步动作的错误。
func (s *Store) Require(applicationID string) (AppConfig, error) {
	cfg, ok, err := s.Get(applicationID)
	if err != nil {
		return AppConfig{}, err
	}
	if !ok {
		id, _ := validateID(applicationID)
		return AppConfig{}, eperr.ConfigurationError(
			"未找到应用配置：%s，请先执行 `easy-publisher app add --id %s`", id, id)
	}
	return cfg, nil
}

// Save 原子地写入配置。
func (s *Store) Save(cfg AppConfig) error {
	id, err := validateID(cfg.ApplicationID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ensureSecureDir(s.dir); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return eperr.ConfigurationError("序列化配置失败：%v", err)
	}
	data = append(data, '\n')

	target := filepath.Join(s.dir, id+fileSuffix)
	tmp, err := os.CreateTemp(s.dir, "."+id+fileSuffix+".tmp*")
	if err != nil {
		return eperr.ConfigurationError("创建临时文件失败：%v", err)
	}
	tmpName := tmp.Name()
	// 临时文件也要 0600：它同样含明文凭据，且创建到 rename 之间有窗口
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return eperr.ConfigurationError("设置临时文件权限失败：%v", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return eperr.ConfigurationError("写入配置失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return eperr.ConfigurationError("关闭临时文件失败：%v", err)
	}
	// 同一目录内 rename 是原子的，读者要么看到旧内容要么看到新内容，不会看到半截
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return eperr.ConfigurationError("替换配置文件失败：%v", err)
	}
	logx.Info("已保存配置", "applicationId", cfg.ApplicationID)
	return nil
}

// Remove 删除配置。先用随机数据覆写再删除，降低明文凭据在磁盘上残留的概率。
//
// 注意：在带日志结构或写时复制的文件系统（APFS、Btrfs、SSD 磨损均衡）上，
// 覆写并不能保证物理擦除。真正敏感的凭据应当在渠道后台轮换。
func (s *Store) Remove(applicationID string) (bool, error) {
	path, err := s.pathFor(applicationID)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, eperr.ConfigurationError("无法读取配置：%v", err)
	}

	if size := st.Size(); size > 0 {
		const maxOverwrite = 1 << 20
		n := size
		if n > maxOverwrite {
			n = maxOverwrite
		}
		noise := make([]byte, n)
		fillRandom(noise)
		if werr := os.WriteFile(path, noise, 0o600); werr != nil {
			logx.Debug("覆写配置文件失败，仍会继续删除", "err", werr)
		}
	}
	if err := os.Remove(path); err != nil {
		return false, eperr.ConfigurationError("删除配置失败：%v", err)
	}
	logx.Info("已删除配置", "applicationId", applicationID)
	return true, nil
}

func (s *Store) read(path string) (AppConfig, error) {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// 上游只 printStackTrace，配置损坏时无迹可查
		logx.Error("配置文件解析失败", "path", path, "err", err)
		return cfg, err
	}
	return cfg, nil
}

func ensureSecureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return eperr.ConfigurationError("创建配置目录失败：%v", err)
	}
	// MkdirAll 的权限受 umask 影响，显式再设一次
	if err := os.Chmod(dir, 0o700); err != nil {
		logx.Debug("设置配置目录权限失败（非 POSIX 文件系统？）", "err", err)
	}
	return nil
}

// idPattern 与 Kotlin 版的 ApplicationId.PATTERN 一致。
var idPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// validateID 校验包名。
//
// 配置文件名直接由包名拼成，未过滤的 / 与 .. 会写到目录外。
// 这里不依赖 artifact 包，避免 config → artifact 的依赖环；两处规则必须保持一致。
func validateID(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", eperr.ConfigurationError("包名不能为空")
	}
	if !idPattern.MatchString(trimmed) {
		return "", eperr.ConfigurationError("包名格式不合法：%s（应形如 com.example.app）", trimmed)
	}
	return trimmed, nil
}

// IsSensitiveKey 报告参数名是否看起来承载敏感值。
func IsSensitiveKey(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range sensitiveHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

var sensitiveHints = []string{
	"secret", "token", "password", "passwd", "private", "key", "sign", "credential",
}
