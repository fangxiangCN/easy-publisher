package vivo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// goldenPath 指向由 Kotlin 侧 GoldenVectorTest 生成的 fixture。
//
// 那批向量是跨语言重写的验收基准：Go 的实现必须对同样输入产出同样的字节。
// fixture 本身已用 Python 的 hmac/hashlib 独立复算过，因此它不是
// 「Kotlin 说自己对」，而是两个独立实现都同意。
const goldenPath = "../../../testdata/golden/signing.json"

type goldenFile struct {
	FixedTimestampMillis int64        `json:"fixedTimestampMillis"`
	Vivo                 []vivoVector `json:"vivo"`
	Oppo                 []oppoVector `json:"oppo"`
}

type vivoVector struct {
	Name           string            `json:"name"`
	AccessKey      string            `json:"accessKey"`
	AccessSecret   string            `json:"accessSecret"`
	Method         string            `json:"method"`
	OriginParams   map[string]string `json:"originParams"`
	TimestampMilli int64             `json:"timestampMillis"`
	Canonical      string            `json:"canonical"`
	Signature      string            `json:"signature"`
	SignedParams   map[string]string `json:"signedParams"`
}

// oppoVector 在本包里只用于确认 fixture 结构未变；OPPO 的断言在它自己的包内。
type oppoVector struct {
	Name      string             `json:"name"`
	Secret    string             `json:"secret"`
	Params    map[string]*string `json:"params"`
	Canonical string             `json:"canonical"`
	Signature string             `json:"signature"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读取黄金向量失败（%v）。在仓库根目录运行以下命令重新生成：\n"+
			"  python3 scripts/gen-golden-vectors.py", err)
	}
	var g goldenFile
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("解析黄金向量失败: %v", err)
	}
	if len(g.Vivo) == 0 {
		t.Fatal("黄金向量里没有 vivo 用例")
	}
	return g
}

// TestSignMatchesGoldenVectors 是 vivo 渠道的核心验收：
// 待签串、签名、完整参数集都必须与 Kotlin 实现逐字节一致。
func TestSignMatchesGoldenVectors(t *testing.T) {
	g := loadGolden(t)

	for _, v := range g.Vivo {
		t.Run(v.Name, func(t *testing.T) {
			got := SignDetailed(v.AccessKey, v.AccessSecret, v.Method, v.OriginParams, v.TimestampMilli)

			if got.Canonical != v.Canonical {
				t.Errorf("待签串不一致\n  Go:     %s\n  golden: %s", got.Canonical, v.Canonical)
			}
			if got.Signature != v.Signature {
				t.Errorf("签名不一致\n  Go:     %s\n  golden: %s", got.Signature, v.Signature)
			}
			if diff := diffMaps(got.Params, v.SignedParams); diff != "" {
				t.Errorf("完整参数集不一致：%s", diff)
			}
		})
	}
}

// TestSignatureIsIndependentHMAC 不复用生产代码的 HMAC，
// 直接用标准库重算一遍并与 fixture 比对。
//
// 这条测试的价值在于它不信任 SignDetailed 的内部实现：
// 如果有人把 hmacSHA256Hex 改错（例如换成 SHA-1、或改了大小写），
// 上一条测试与这一条会同时失败，从而区分「拼串错了」与「HMAC 错了」。
func TestSignatureIsIndependentHMAC(t *testing.T) {
	g := loadGolden(t)
	for _, v := range g.Vivo {
		mac := hmac.New(sha256.New, []byte(v.AccessSecret))
		mac.Write([]byte(v.Canonical))
		want := hex.EncodeToString(mac.Sum(nil))
		if want != v.Signature {
			t.Errorf("%s: 独立重算的签名与 fixture 不符\n  重算:   %s\n  fixture: %s",
				v.Name, want, v.Signature)
		}
	}
}

func TestCanonicalizeRules(t *testing.T) {
	// 这几条断言独立于 fixture，直接表达算法约定。
	// fixture 比对能发现「变了」，这几条能说明「变成了什么」
	t.Run("按字典序排序与传入顺序无关", func(t *testing.T) {
		got := Canonicalize(map[string]string{
			"timestamp": "1", "access_token": "tok", "pkg_name": "com.example",
		})
		want := "access_token=tok&pkg_name=com.example&timestamp=1"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("空值仍参与，与键不存在不同", func(t *testing.T) {
		got := Canonicalize(map[string]string{"a": "", "b": "2"})
		if got != "a=&b=2" {
			t.Errorf("got %q, want %q", got, "a=&b=2")
		}
	})

	t.Run("值不做 URL 编码", func(t *testing.T) {
		got := Canonicalize(map[string]string{"d": "修复 A&B=C 的问题，支持中文"})
		want := "d=修复 A&B=C 的问题，支持中文"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("空参数集产生空串", func(t *testing.T) {
		if got := Canonicalize(nil); got != "" {
			t.Errorf("got %q, want 空串", got)
		}
	})
}

func TestSignAddsAllCommonParams(t *testing.T) {
	res := SignDetailed("ak", "sk", "app.sync.update.app",
		map[string]string{"packageName": "com.example"}, 1700000000000)

	for _, key := range []string{
		paramAccessKey, paramTimestamp, paramMethod, paramVersion,
		paramSignMethod, paramFormat, paramTargetAppKey,
	} {
		if _, ok := res.Params[key]; !ok {
			t.Errorf("缺少公共参数 %s", key)
		}
		if !strings.Contains(res.Canonical, key+"=") {
			t.Errorf("公共参数 %s 必须参与待签串", key)
		}
	}

	// sign 自身不能参与待签串，否则无法计算
	if strings.Contains(res.Canonical, "sign=") {
		t.Errorf("sign 不应参与待签串：%s", res.Canonical)
	}
	if res.Params[paramSign] != res.Signature {
		t.Error("返回的 params 里应当含 sign")
	}
	if res.Params[paramTimestamp] != "1700000000000" {
		t.Errorf("timestamp = %q, 应为毫秒", res.Params[paramTimestamp])
	}
	if res.Params[paramVersion] != "1.0" ||
		res.Params[paramSignMethod] != "HMAC-SHA256" ||
		res.Params[paramFormat] != "json" ||
		res.Params[paramTargetAppKey] != "developer" {
		t.Errorf("公共参数取值不对：%v", res.Params)
	}
}

func TestSignDoesNotMutateInput(t *testing.T) {
	origin := map[string]string{"packageName": "com.example"}
	SignDetailed("ak", "sk", "m", origin, 1)

	// 调用方的 map 不能被改写：同一个 params 可能被多个请求复用，
	// 而 Go 的 map 是引用类型，直接往里写会污染调用方
	if len(origin) != 1 {
		t.Errorf("入参被改写：%v", origin)
	}
	if _, ok := origin[paramSign]; ok {
		t.Error("入参里被写入了 sign")
	}
}

func TestSignatureIsLowercaseHex64(t *testing.T) {
	res := SignDetailed("ak", "sk", "m", nil, 1)
	if len(res.Signature) != 64 {
		t.Errorf("签名长度 = %d, 期望 64（SHA-256 的十六进制）", len(res.Signature))
	}
	if res.Signature != strings.ToLower(res.Signature) {
		t.Errorf("签名必须是小写：%s", res.Signature)
	}
}

func diffMaps(got, want map[string]string) string {
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var diffs []string
	for _, k := range sorted {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case gok && !wok:
			diffs = append(diffs, "多出 "+k+"="+g)
		case !gok && wok:
			diffs = append(diffs, "缺少 "+k+"="+w)
		case g != w:
			diffs = append(diffs, k+": Go="+g+" golden="+w)
		}
	}
	return strings.Join(diffs, "; ")
}

// 确认 fixture 路径没写错，避免测试因为找不到文件而静默跳过
func TestGoldenFixtureExists(t *testing.T) {
	abs, err := filepath.Abs(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("黄金向量 fixture 不在预期位置 %s: %v", abs, err)
	}
}
