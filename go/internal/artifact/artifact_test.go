package artifact

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// APK fixture 是用 Android SDK 的 aapt2 link 从文本 manifest 真实构建出来的，
// 因此里面的 AndroidManifest.xml 是真正的二进制 AXML，不是手搓的假数据。
// 期望值取自 `aapt2 dump badging`，即 Android 官方工具的判定 ——
// 这样测的是「我们的解析与官方工具一致」，而不只是「与自己一致」。
//
// 同一批 fixture 也被 Kotlin 侧的 ApkParserParityTest 使用，
// 用于比对 net.dongliu:apk-parser 与 avast/apkparser 两个库的行为。
const apkDir = "../../testdata/apk"

func TestReadAPK(t *testing.T) {
	cases := []struct {
		file        string
		appID       string
		versionCode int64
		versionName string
	}{
		{"basic.apk", "com.example.goldentest", 1020, "1.2.0"},
		// manifest 里没有 android:versionName 时，aapt2 报 versionName=''
		{"no-version-name.apk", "com.example.onlycode", 7, ""},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			info, err := Read(filepath.Join(apkDir, tc.file))
			if err != nil {
				t.Fatalf("Read 失败: %v", err)
			}
			if info.Kind != KindAPK {
				t.Errorf("Kind = %v, 期望 APK", info.Kind)
			}
			if info.ApplicationID != tc.appID {
				t.Errorf("ApplicationID = %q, 期望 %q", info.ApplicationID, tc.appID)
			}
			if info.VersionCode != tc.versionCode {
				t.Errorf("VersionCode = %d, 期望 %d", info.VersionCode, tc.versionCode)
			}
			if info.VersionName != tc.versionName {
				t.Errorf("VersionName = %q, 期望 %q", info.VersionName, tc.versionName)
			}
			if info.SizeBytes <= 0 {
				t.Errorf("SizeBytes = %d, 应为正数", info.SizeBytes)
			}
		})
	}
}

// 期望值必须与 aapt2 dump badging 的输出一致。
// 这条测试把 fixture 与官方工具的输出绑在一起：将来换了 fixture 却忘了更新
// 断言，或者 aapt2 的输出与我们的解析出现分歧，都会在这里暴露。
func TestAPKMatchesAapt2Badging(t *testing.T) {
	entries, err := os.ReadDir(apkDir)
	if err != nil {
		t.Fatalf("读取 fixture 目录失败: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".apk") {
			continue
		}
		badgingPath := filepath.Join(apkDir, strings.TrimSuffix(e.Name(), ".apk")+".badging.txt")
		raw, err := os.ReadFile(badgingPath)
		if err != nil {
			t.Errorf("%s 缺少对应的 aapt2 基准文件 %s", e.Name(), filepath.Base(badgingPath))
			continue
		}
		info, err := Read(filepath.Join(apkDir, e.Name()))
		if err != nil {
			t.Errorf("Read(%s) 失败: %v", e.Name(), err)
			continue
		}
		line := string(raw)
		for _, want := range []string{
			"name='" + info.ApplicationID + "'",
			"versionCode='" + itoa(info.VersionCode) + "'",
			"versionName='" + info.VersionName + "'",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: 解析结果与 aapt2 基准不符\n  我们得到: %s\n  aapt2 说: %s",
					e.Name(), want, strings.TrimSpace(line))
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没有找到任何 APK fixture")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ---- App Pack ----

// writeAppPack 构造一个 .app（本质是 zip），packInfo 为 nil 时不写入 pack.info。
func writeAppPack(t *testing.T, dir, name string, packInfo any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 fixture 失败: %v", err)
	}
	zw := zip.NewWriter(f)
	write := func(entry string, data []byte) {
		w, err := zw.Create(entry)
		if err != nil {
			t.Fatalf("写入 %s 失败: %v", entry, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("写入 %s 失败: %v", entry, err)
		}
	}
	write("module.json", []byte("{}"))
	if packInfo != nil {
		data, err := json.Marshal(packInfo)
		if err != nil {
			t.Fatalf("序列化 pack.info 失败: %v", err)
		}
		write("pack.info", data)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭文件失败: %v", err)
	}
	return path
}

func packInfoOf(bundleName string, code *int64, name string) map[string]any {
	version := map[string]any{}
	if code != nil {
		version["code"] = *code
	}
	if name != "" {
		version["name"] = name
	}
	app := map[string]any{"version": version}
	if bundleName != "" {
		app["bundleName"] = bundleName
	}
	return map[string]any{"summary": map[string]any{
		"app":     app,
		"modules": []any{map[string]any{"name": "entry"}},
	}}
}

func i64(v int64) *int64 { return &v }

func TestReadAppPack(t *testing.T) {
	dir := t.TempDir()
	path := writeAppPack(t, dir, "demo.app", packInfoOf("com.example.harmony", i64(1000012), "1.0.12"))

	info, err := Read(path)
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if info.Kind != KindHarmonyAppPack {
		t.Errorf("Kind = %v, 期望 App Pack", info.Kind)
	}
	if info.ApplicationID != "com.example.harmony" {
		t.Errorf("ApplicationID = %q", info.ApplicationID)
	}
	if info.VersionCode != 1000012 {
		t.Errorf("VersionCode = %d", info.VersionCode)
	}
	if info.VersionName != "1.0.12" {
		t.Errorf("VersionName = %q", info.VersionName)
	}
}

func TestReadAppPackVersionNameFallsBackToCode(t *testing.T) {
	dir := t.TempDir()
	path := writeAppPack(t, dir, "demo.app", packInfoOf("com.example.harmony", i64(1000012), ""))

	info, err := Read(path)
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if info.VersionName != "1000012" {
		t.Errorf("VersionName = %q, 期望回退为 versionCode 的字符串形式", info.VersionName)
	}
}

func TestReadAppPackErrors(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name       string
		packInfo   any
		wantSubstr string
	}{
		{
			name:     "缺少 pack.info",
			packInfo: nil,
			// 单个 .hap 没有 pack.info，这是最常见的误用，提示里应当点到
			wantSubstr: ".hap",
		},
		{
			name:       "缺少 bundleName",
			packInfo:   packInfoOf("", i64(1), "1.0"),
			wantSubstr: "bundleName",
		},
		{
			// PublishPolicy 的版本比对依赖 versionCode。拿不到就报错，
			// 不能静默降级成 0 或跳过校验却让人以为已经检查过了
			name:       "缺少 version.code",
			packInfo:   packInfoOf("com.example.harmony", nil, "1.0"),
			wantSubstr: "version.code",
		},
		{
			// HSP 的 pack.info 不含 summary/app，属于真实会出现的形状
			name:       "只有 modules",
			packInfo:   map[string]any{"summary": map[string]any{"modules": []any{}}},
			wantSubstr: "bundleName",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.packInfo == nil {
				path = writeAppPack(t, dir, "nopack.app", nil)
			} else {
				path = writeAppPack(t, dir, tc.name+".app", tc.packInfo)
			}
			_, err := Read(path)
			if err == nil {
				t.Fatal("期望失败，实际成功")
			}
			var pe *eperr.Error
			if !errors.As(err, &pe) {
				t.Fatalf("错误类型不是 *eperr.Error: %T", err)
			}
			if pe.Kind != eperr.KindLocalFile {
				t.Errorf("Kind = %v, 期望 LocalFile", pe.Kind)
			}
			if !strings.Contains(pe.Msg, tc.wantSubstr) {
				t.Errorf("错误信息应包含 %q，实际：%s", tc.wantSubstr, pe.Msg)
			}
		})
	}
}

func TestReadRejectsHap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry-default.hap")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	if err == nil {
		t.Fatal("期望失败：.hap 不是商店发布包")
	}
	if !strings.Contains(err.Error(), "hap") {
		t.Errorf("应当回显收到的格式，实际：%v", err)
	}
}

func TestReadRejectsEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.app")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(empty); err == nil || !strings.Contains(err.Error(), "为空") {
		t.Errorf("空文件应报「为空」，实际：%v", err)
	}

	if _, err := Read(filepath.Join(dir, "absent.apk")); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Errorf("不存在的文件应报「不存在」，实际：%v", err)
	}

	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "不是文件") {
		t.Errorf("目录应报「不是文件」，实际：%v", err)
	}
}

func TestKindOfExtension(t *testing.T) {
	cases := []struct {
		ext  string
		want Kind
		ok   bool
	}{
		{"apk", KindAPK, true},
		{".apk", KindAPK, true},
		{"APK", KindAPK, true},
		{"app", KindHarmonyAppPack, true},
		{"hap", KindAPK, false},
		{"aab", KindAPK, false},
		{"", KindAPK, false},
	}
	for _, tc := range cases {
		got, ok := KindOfExtension(tc.ext)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("KindOfExtension(%q) = (%v, %v), 期望 (%v, %v)", tc.ext, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidateApplicationID(t *testing.T) {
	valid := []string{"com.example.app", "com.a.b", "com.example.my_app2"}
	for _, v := range valid {
		if _, err := ValidateApplicationID(v); err != nil {
			t.Errorf("%q 应当合法: %v", v, err)
		}
	}

	// 配置文件名直接由包名拼成，未过滤的 / 与 .. 会写到目录外
	invalid := []string{"", "  ", "com", ".com.example", "com..example",
		"../../etc/passwd", "com.example/app", "com.example app", "1com.example"}
	for _, v := range invalid {
		if _, err := ValidateApplicationID(v); err == nil {
			t.Errorf("%q 应当被拒绝", v)
		}
	}
}
