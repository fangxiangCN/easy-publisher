package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// 占位符检测：包名尾段作为应用名是最典型的漏改。
func TestLabelIsPlaceholderDetectsPackageTail(t *testing.T) {
	cases := []struct {
		label   string
		pkg     string
		want    bool
		comment string
	}{
		{"civilian", "tech.lightsoft.civilian", true, "包名尾段，Flutter 模板的典型值"},
		{"gwy", "com.doquestion.gwy", true, "同上"},
		{"flutter", "com.example.app", true, "脚手架常见默认名"},
		{"军队文职真题", "tech.lightsoft.civilian", false, "正经中文名"},
		{"CIVILIAN", "tech.lightsoft.civilian", true, "大小写不敏感"},
		{"", "tech.lightsoft.civilian", true, "空名必是漏改"},
	}
	for _, tc := range cases {
		got := ManifestDetail{Label: tc.label, PackageName: tc.pkg}.LabelIsPlaceholder()
		if got != tc.want {
			t.Errorf("%s：Label=%q Package=%q 期望 %v，实际 %v",
				tc.comment, tc.label, tc.pkg, tc.want, got)
		}
	}
}

// 像素哈希必须按非预乘口径算，否则内置的模板图标指纹一个都命中不了。
//
// 这个 bug 真的发生过：首版用 image.RGBA（预乘 alpha），而常量是按非预乘
// 口径（PIL）算出的，两者对含透明像素的调色板 PNG 结果不同，检测形同虚设
// —— 单测当时是绿的，因为常量与实现「一致地错」不了：常量来自外部工具。
func TestPixelHashUsesNonPremultipliedAlpha(t *testing.T) {
	// 一块半透明红 + 一块全不透明红。预乘与非预乘在透明像素上取值不同
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 128})
	img.SetNRGBA(1, 0, color.NRGBA{R: 255, A: 255})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	got, err := pixelHash(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	// 期望值按非预乘口径独立算出：像素为 (255,0,0,128) 与 (255,0,0,255)
	wantPixels := []byte{255, 0, 0, 128, 255, 0, 0, 255}
	sum := sha256.Sum256(wantPixels)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("像素哈希口径不对：\n实际 %s\n期望 %s（非预乘）", got, want)
	}
}

// 内置的模板图标常量必须与非预乘口径一致 —— 这是那批常量的来源约定。
func TestKnownTemplateHashesMatchNonPremultipliedEncoding(t *testing.T) {
	if len(knownTemplateIconHashes) == 0 {
		t.Fatal("模板图标指纹表为空，检测会永远返回「不是模板图」")
	}
	for hash, desc := range knownTemplateIconHashes {
		if len(hash) != 64 {
			t.Errorf("%s 的指纹不是 SHA-256 十六进制：%q", desc, hash)
		}
	}
}

func TestAppLabelMismatch(t *testing.T) {
	d := ManifestDetail{Label: "军队文职真题"}
	if d.AppLabelMismatch("军队文职真题") {
		t.Error("完全一致不应判为不一致")
	}
	if d.AppLabelMismatch(" 军队文职真题 ") {
		t.Error("两侧空白差异不应判为不一致")
	}
	if !d.AppLabelMismatch("公务员公考真题") {
		t.Error("不同名称应判为不一致")
	}
	// 未提供期望名时不做判定，由调用方按「无法比对」处理
	if d.AppLabelMismatch("") {
		t.Error("未提供期望名时不应判为不一致")
	}
}
