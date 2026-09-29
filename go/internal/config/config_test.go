package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

func sample(id string) AppConfig {
	return AppConfig{
		Name:          "示例应用",
		ApplicationID: id,
		CreateTime:    1700000000000,
		Channels: []ChannelConfig{
			{
				Name:    "huawei",
				Enabled: true,
				Params: []Param{
					{Name: "client_id", Value: "id-123"},
					{Name: "client_secret", Value: "secret-456"},
				},
			},
		},
	}
}

func TestStoreSaveAndRead(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Save(sample("com.example.app")); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	cfg, ok, err := s.Get("com.example.app")
	if err != nil || !ok {
		t.Fatalf("Get 失败: ok=%v err=%v", ok, err)
	}
	if cfg.Name != "示例应用" {
		t.Errorf("Name = %q", cfg.Name)
	}
	ch, found := cfg.Channel("huawei")
	if !found {
		t.Fatal("未找到 huawei 渠道配置")
	}
	v, ok := ch.Value("client_secret")
	if !ok || v != "secret-456" {
		t.Errorf("client_secret = %q, %v", v, ok)
	}
}

func TestConfigFilePermissions(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(sample("com.example.app")); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "com.example.app.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 凭据文件必须只有所有者可读写，否则同机其他用户能读到各商店密钥
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("配置文件权限 = %04o, 期望 0600", perm)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 目录权限：TempDir 建的时候是 0700，Save 里的 ensureSecureDir 应保持它
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("配置目录权限 = %04o, 不应对同组或其他用户开放", perm)
	}
}

func TestRemoveActuallyDeletesAndLeavesNoBak(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(sample("com.example.app")); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Remove("com.example.app")
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	if _, ok, _ := s.Get("com.example.app"); ok {
		t.Error("删除后仍能读到配置")
	}

	// 上游实现只把文件改名为 .bak，明文凭据仍留在磁盘上
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "com.example.app") {
			t.Errorf("删除后仍有残留文件：%s（上游就是 rename 成 .bak 而不真删）", e.Name())
		}
	}
}

func TestRemoveMissingReturnsFalse(t *testing.T) {
	s := NewStore(t.TempDir())
	removed, err := s.Remove("com.example.absent")
	if err != nil {
		t.Fatalf("不存在的配置不应报错: %v", err)
	}
	if removed {
		t.Error("应返回 false")
	}
}

func TestSaveIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(sample("com.example.app")); err != nil {
		t.Fatal(err)
	}
	cfg := sample("com.example.app")
	cfg.Name = "改名后"
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.Get("com.example.app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "改名后" {
		t.Errorf("Name = %q", got.Name)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("临时文件未清理：%s", e.Name())
		}
	}
}

func TestSaveRejectsInvalidApplicationID(t *testing.T) {
	s := NewStore(t.TempDir())
	// 配置文件名直接由包名拼成，未过滤的 ../ 会写到目录外
	err := s.Save(sample("../../etc/passwd"))
	if err == nil {
		t.Fatal("非法包名应当被拒绝")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration 错误，实际 %v", err)
	}
}

func TestRequireGivesActionableMessage(t *testing.T) {
	s := NewStore(t.TempDir())
	_, err := s.Require("com.example.absent")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "app add") {
		t.Errorf("提示应包含下一步动作，实际：%v", err)
	}
}

func TestCorruptConfigIsReportedNotSilentlyDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "com.example.app.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)

	// 上游的实现是 printStackTrace 后返回 null，配置「凭空消失」且无迹可查
	_, _, err := s.Get("com.example.app")
	if err == nil {
		t.Error("损坏的配置应当报错，而不是静默当成不存在")
	}
}

func TestCredentialsNeverAppearInStringers(t *testing.T) {
	cfg := sample("com.example.app")
	dumped := cfg.String() + " " + cfg.Channels[0].String() + " " +
		Param{Name: "client_secret", Value: "secret-456"}.String()

	if strings.Contains(dumped, "secret-456") {
		t.Errorf("String() 泄漏了凭据，上游正是这样把密钥打进日志的：%s", dumped)
	}
	if !strings.Contains(dumped, "client_secret") {
		t.Errorf("参数名应当保留，便于排查：%s", dumped)
	}
}

// ---- 凭据读取 ----

func TestEnvOverridesConfigFile(t *testing.T) {
	cfg := sample("com.example.app")
	env := NewEnvStoreFrom(map[string]string{
		"EP_HUAWEI_CLIENT_SECRET": "from-env",
	})
	store := NewLayeredStore(cfg, env)

	// 环境变量优先：CI 里临时覆盖某个参数不必改动磁盘上的配置文件
	v, ok, err := store.Get("com.example.app", "huawei", "client_secret")
	if err != nil || !ok || v != "from-env" {
		t.Errorf("期望环境变量生效，得到 %q ok=%v err=%v", v, ok, err)
	}

	// 未覆盖的参数仍从配置文件读
	v, ok, err = store.Get("com.example.app", "huawei", "client_id")
	if err != nil || !ok || v != "id-123" {
		t.Errorf("期望配置文件生效，得到 %q ok=%v err=%v", v, ok, err)
	}
}

func TestEmptyValueTreatedAsUnset(t *testing.T) {
	cfg := sample("com.example.app")
	cfg.Channels[0].Params = append(cfg.Channels[0].Params, Param{Name: "blank", Value: "   "})
	store := NewLayeredStore(cfg, NewEnvStoreFrom(map[string]string{
		"EP_HUAWEI_FROM_ENV": "  ",
	}))

	// 否则一个 --value "" 就能把必填校验绕过去，直到请求发出才失败
	if _, ok, _ := store.Get("com.example.app", "huawei", "blank"); ok {
		t.Error("配置文件里的空白值应视为未配置")
	}
	if _, ok, _ := store.Get("com.example.app", "huawei", "from_env"); ok {
		t.Error("环境变量里的空白值应视为未配置")
	}
}

func TestRequireSameAppRejectsMismatch(t *testing.T) {
	cfg := sample("com.example.app")
	store := NewLayeredStore(cfg, NewEnvStoreFrom(nil))

	// 防「用 A 应用的密钥发布 B 应用的包」—— 上游进程级单例持有可变凭据的事故类型
	_, _, err := store.Get("com.other.app", "huawei", "client_id")
	if err == nil {
		t.Fatal("配置归属不一致应当立刻失败")
	}
	var pe *eperr.Error
	if !errors.As(err, &pe) || pe.Kind != eperr.KindConfiguration {
		t.Errorf("期望 Configuration 错误，实际 %v", err)
	}
	if strings.Contains(err.Error(), "id-123") {
		t.Errorf("错误信息不得含凭据值：%v", err)
	}
}

func TestRequireGivesEnvVarHint(t *testing.T) {
	cfg := sample("com.example.app")
	store := NewLayeredStore(cfg, NewEnvStoreFrom(nil))

	_, err := Require(store, "com.example.app", "huawei", "missing_param")
	if err == nil {
		t.Fatal("缺失的必填参数应当报错")
	}
	if !strings.Contains(err.Error(), "EP_HUAWEI_MISSING_PARAM") {
		t.Errorf("提示应给出环境变量名，实际：%v", err)
	}
}

func TestEnvNameNormalization(t *testing.T) {
	cases := map[[2]string]string{
		{"huawei", "client_secret"}: "EP_HUAWEI_CLIENT_SECRET",
		{"mi", "publicKey"}:         "EP_MI_PUBLIC_KEY", // 驼峰要切开，否则用户得猜 PUBLICKEY
		{"mi", "privateKey"}:        "EP_MI_PRIVATE_KEY",
		{"oppo", "client_id"}:       "EP_OPPO_CLIENT_ID",
		{"harmony", "app_id"}:       "EP_HARMONY_APP_ID",
		{"vivo", "access-key"}:      "EP_VIVO_ACCESS_KEY", // 非字母数字转下划线
		{"mi", "some__weird..name"}: "EP_MI_SOME_WEIRD_NAME",
	}
	for in, want := range cases {
		if got := EnvName(in[0], in[1]); got != want {
			t.Errorf("EnvName(%q, %q) = %q, 期望 %q", in[0], in[1], got, want)
		}
	}
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{"client_secret", "access_token", "privateKey", "api_sign", "password", "credential"}
	for _, k := range sensitive {
		if !IsSensitiveKey(k) {
			t.Errorf("%q 应判定为敏感", k)
		}
	}
	plain := []string{"client_id", "account", "app_id", "packageName"}
	for _, k := range plain {
		if IsSensitiveKey(k) {
			t.Errorf("%q 不应判定为敏感", k)
		}
	}
}

func TestConfigJSONIsInterchangeableWithKotlin(t *testing.T) {
	// 字段名必须与 Kotlin 版逐字一致，两边的配置文件才能互换
	data, err := json.Marshal(sample("com.example.app"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "applicationId", "createTime", "channels", "multiChannelApk"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("缺少字段 %q，Kotlin 版读不了这个配置文件。实际字段：%v", key, keysOf(raw))
		}
	}

	channels := raw["channels"].([]any)
	ch := channels[0].(map[string]any)
	for _, key := range []string{"name", "enabled", "params"} {
		if _, ok := ch[key]; !ok {
			t.Errorf("channel 缺少字段 %q", key)
		}
	}
	params := ch["params"].([]any)
	p := params[0].(map[string]any)
	for _, key := range []string{"name", "value"} {
		if _, ok := p[key]; !ok {
			t.Errorf("param 缺少字段 %q", key)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
