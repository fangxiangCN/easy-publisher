package config

import (
	"crypto/rand"
	"os"
	"regexp"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
)

// fillRandom 用加密安全随机数填充缓冲区。
//
// 用 crypto/rand 而不是 math/rand：目的是让被删除的凭据在磁盘上不可辨认，
// 可预测的填充等于没填。填充失败时退化为固定模式 —— 删除本身仍然要继续，
// 不能因为无法安全擦除就把文件留在原地。
func fillRandom(b []byte) {
	if _, err := rand.Read(b); err == nil {
		return
	}
	for i := range b {
		b[i] = byte(i)
	}
}

// CredentialStore 是渠道凭据的来源。
//
// 凭据是**程序自己读**的，不是调用方传进来的 —— CLI 与 MCP 的对外接口都不接受
// secret 参数。这样密钥不会进入 shell 历史、CI 日志，也不会进入 AI 模型的对话上下文。
type CredentialStore interface {
	// Get 读取一个渠道参数。未配置或值为空时返回 ok=false。
	//
	// 空字符串一律视为「未配置」，否则一个 --value "" 就能把必填校验绕过去，
	// 直到请求发出才失败。
	Get(applicationID, channelID, paramName string) (string, bool, error)
}

// Require 读取一个必填参数，缺失时给出可执行的提示。
func Require(store CredentialStore, applicationID, channelID, paramName string) (string, error) {
	value, ok, err := store.Get(applicationID, channelID, paramName)
	if err != nil {
		return "", err
	}
	if ok {
		return value, nil
	}
	return "", publish.CredentialError(
		"缺少凭据参数 %s，可通过环境变量 %s 或 "+
			"`easy-publisher channel set --app %s --channel %s --key %s --value <值>` 配置",
		paramName, EnvName(channelID, paramName), applicationID, channelID, paramName,
	)
}

// ConfigStore 从已加载的应用配置里读凭据。
type ConfigStore struct {
	config AppConfig
}

func NewConfigStore(config AppConfig) *ConfigStore { return &ConfigStore{config: config} }

func (s *ConfigStore) Get(applicationID, channelID, paramName string) (string, bool, error) {
	if err := requireSameApp(applicationID, s.config); err != nil {
		return "", false, err
	}
	ch, ok := s.config.Channel(channelID)
	if !ok {
		return "", false, nil
	}
	value, ok := ch.Value(paramName)
	return value, ok, nil
}

// EnvStore 从环境变量读凭据，格式 EP_<渠道>_<参数>，例如 EP_HUAWEI_CLIENT_SECRET。
//
// CI 场景可以完全不落盘，凭据只存在于进程环境里。
//
// 环境变量是**进程级**的，不区分应用 —— 若同一个环境里发布多个应用、而它们同名参数
// 取值不同，环境变量会覆盖所有应用。多应用场景请用配置文件。
type EnvStore struct {
	env map[string]string
}

// NewEnvStore 用进程环境变量构造。
func NewEnvStore() *EnvStore { return &EnvStore{env: environMap()} }

// NewEnvStoreFrom 用给定的映射构造，便于测试。
func NewEnvStoreFrom(env map[string]string) *EnvStore { return &EnvStore{env: env} }

func (s *EnvStore) Get(_, channelID, paramName string) (string, bool, error) {
	value := strings.TrimSpace(s.env[EnvName(channelID, paramName)])
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

// EnvName 把渠道与参数名映射成环境变量名。
//
// client_secret → EP_HUAWEI_CLIENT_SECRET；驼峰也切成下划线，
// publicKey → EP_MI_PUBLIC_KEY，免得用户要猜是 PUBLICKEY 还是 PUBLIC_KEY。
// `channel list` 会逐个参数打印这个名字，不必自己推导。
func EnvName(channelID, paramName string) string {
	return "EP_" + normalizeEnvPart(channelID) + "_" + normalizeEnvPart(paramName)
}

var (
	nonAlnumRun   = regexp.MustCompile(`[^A-Za-z0-9]+`)
	camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	underscoreRun = regexp.MustCompile(`_+`)
)

func normalizeEnvPart(raw string) string {
	s := nonAlnumRun.ReplaceAllString(raw, "_")
	s = camelBoundary.ReplaceAllString(s, "${1}_${2}")
	s = strings.ToUpper(s)
	s = underscoreRun.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// LayeredStore 组合两种来源，**环境变量优先**。
//
// 顺序是刻意的：CI 里临时覆盖某个参数（例如换用测试环境的 client_secret）
// 不必改动磁盘上的配置文件。反过来若配置文件优先，环境变量形同虚设，
// 而使用者会以为已经生效 —— 这种「以为改了其实没改」比缺参数更难排查，
// 因为它表现为拿旧凭据静默失败。
type LayeredStore struct {
	config AppConfig
	env    *EnvStore
	file   *ConfigStore
}

func NewLayeredStore(config AppConfig, env *EnvStore) *LayeredStore {
	if env == nil {
		env = NewEnvStore()
	}
	return &LayeredStore{config: config, env: env, file: NewConfigStore(config)}
}

func (s *LayeredStore) Get(applicationID, channelID, paramName string) (string, bool, error) {
	if err := requireSameApp(applicationID, s.config); err != nil {
		return "", false, err
	}
	if value, ok, err := s.env.Get(applicationID, channelID, paramName); err != nil {
		return "", false, err
	} else if ok {
		// 只记参数名与来源，绝不记值
		logx.Debug("凭据取自环境变量", "channel", channelID, "param", paramName)
		return value, true, nil
	}
	value, ok, err := s.file.Get(applicationID, channelID, paramName)
	if err != nil {
		return "", false, err
	}
	if ok {
		logx.Debug("凭据取自配置文件", "channel", channelID, "param", paramName)
	}
	return value, ok, nil
}

// requireSameApp 校验配置对象的归属。
//
// 不一致说明调用方传错了配置，继续执行会用错应用的密钥 —— 宁可立刻失败。
// 这正是上游进程级单例持有可变 clientId/clientSecret 导致的事故类型。
// 消息里只出现包名，不含任何凭据值。
func requireSameApp(applicationID string, config AppConfig) error {
	if applicationID != config.ApplicationID {
		return publish.ConfigurationError(
			"凭据来源与目标应用不一致：请求 %s，持有 %s。这通常意味着编排层传错了配置对象",
			applicationID, config.ApplicationID,
		)
	}
	return nil
}

func environMap() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}
