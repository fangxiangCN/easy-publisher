package artifact

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/avast/apkparser"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// ManifestDetail 是从 APK 的 AndroidManifest.xml 里读到的、与上架合规相关的字段。
//
// 与 [Read] 的差别：Read 只要包名与版本号，读完 <manifest> 根元素就提前结束；
// 这里要读到 <application> 才能拿到应用名、图标与明文流量配置，因此解析更多节点。
// 上架前的检查需要这些字段 —— 应用名与图标不一致、明文流量被拦，
// 都是渠道实际驳回过的问题。
type ManifestDetail struct {
	PackageName string
	VersionCode int64
	VersionName string
	Label       string
	IconPath    string
	MinSDK      int
	TargetSDK   int
	// CleartextTraffic 是 android:usesCleartextTraffic 的显式声明；未声明时为 nil。
	CleartextTraffic *bool
	// NetworkSecurityConfig 是 android:networkSecurityConfig 指向的文件在 APK 内的路径，
	// 未声明时为空。
	NetworkSecurityConfig string
}

// LabelIsPlaceholder 判断应用名是否还是脚手架生成的占位符。
//
// 背景：本项目从 Flutter 模板起步，android:label 长期是 "civilian" / "gwy"
// 这类包名尾段。渠道会拿它与商店页的应用名比对，不一致即驳回
// （华为实测报 "AppName is not same as it in apk package"）。
//
// 判定方式：与应用名规范化后的包名尾段相同，或包含脚手架常见词。
// 这是启发式 —— 名为 "civilian" 的正经应用会被误报，但那种命名在
// 国内应用市场本来就过不了审，宁可提示。
func (m ManifestDetail) LabelIsPlaceholder() bool {
	label := strings.ToLower(strings.TrimSpace(m.Label))
	if label == "" {
		return true
	}
	segments := strings.Split(strings.ToLower(m.PackageName), ".")
	if len(segments) > 0 {
		if last := segments[len(segments)-1]; last != "" && label == last {
			return true
		}
	}
	for _, word := range []string{"flutter", "example", "myapp", "untitled", "app"} {
		if label == word {
			return true
		}
	}
	return false
}

// AppLabelMismatch 判断 APK 内的应用名与期望名是否不一致（忽略空白差异）。
func (m ManifestDetail) AppLabelMismatch(expected string) bool {
	exp := strings.TrimSpace(expected)
	if exp == "" {
		return false
	}
	return strings.TrimSpace(m.Label) != exp
}

// detailScanner 收集 manifest 里与合规检查相关的节点。
//
// 与 manifestScanner 不同，这里不提前结束：targetSdk 与 application 的先后
// 顺序在不同工具链下并不固定，读完整份 manifest 最稳妥（文件本身很小）。
//
// android:label 这类资源引用（@string/app_name）由 apkparser 在解析阶段展开，
// 拿到的是实际字符串；展开失败时是 @0x7f... 形式，由上层按「无法确认」处理。
type detailScanner struct {
	detail ManifestDetail
}

func (s *detailScanner) EncodeToken(t xml.Token) error {
	switch node := t.(type) {
	case xml.StartElement:
		switch node.Name.Local {
		case "manifest":
			for _, a := range node.Attr {
				switch a.Name.Local {
				case "package":
					s.detail.PackageName = a.Value
				case "versionCode":
					s.detail.VersionCode, _ = strconv.ParseInt(a.Value, 10, 64)
				case "versionName":
					s.detail.VersionName = a.Value
				}
			}
		case "uses-sdk":
			for _, a := range node.Attr {
				switch a.Name.Local {
				case "minSdkVersion":
					s.detail.MinSDK, _ = strconv.Atoi(a.Value)
				case "targetSdkVersion":
					s.detail.TargetSDK, _ = strconv.Atoi(a.Value)
				}
			}
		case "application":
			for _, a := range node.Attr {
				switch a.Name.Local {
				case "label":
					s.detail.Label = a.Value
				case "icon":
					s.detail.IconPath = a.Value
				case "usesCleartextTraffic":
					if v, err := strconv.ParseBool(a.Value); err == nil {
						s.detail.CleartextTraffic = &v
					}
				case "networkSecurityConfig":
					s.detail.NetworkSecurityConfig = a.Value
				}
			}
		}
	}
	return nil
}

func (s *detailScanner) Flush() error { return nil }

// ReadManifestDetail 读取 APK 的 manifest 详情。
//
// 仅支持 APK：App Pack 的元信息在 pack.info 里，没有等价的应用名/图标字段，
// 那些检查对鸿蒙制品不适用（它的名称与图标由 AGC 后台素材决定）。
func ReadManifestDetail(path string) (ManifestDetail, error) {
	if kind, ok := KindOfFile(path); !ok || kind != KindAPK {
		return ManifestDetail{}, eperr.LocalFileError(
			"只有 APK 支持这项检查，%s 不是 APK（鸿蒙的应用名与图标由 AGC 后台素材决定）",
			path)
	}

	scanner := &detailScanner{}
	// 与 readAPK 同理：只关心 manifest，资源表错误可以忽略
	// （资源表解析失败时 manifest 里的引用不会被展开，属性会保留 @0x7f... 形式）
	zipErr, _, manErr := apkparser.ParseApk(path, scanner)
	if zipErr != nil {
		return ManifestDetail{}, eperr.LocalFileError("打开 APK 失败：%s（%v）", path, zipErr)
	}
	if manErr != nil {
		return ManifestDetail{}, eperr.LocalFileError(
			"解析 APK 的 AndroidManifest 失败：%s（%v）", path, manErr)
	}
	if scanner.detail.PackageName == "" {
		return ManifestDetail{}, eperr.LocalFileError(
			"APK 的 AndroidManifest 里没有 package 属性：%s", path)
	}
	return scanner.detail, nil
}

// ---- 图标指纹 ----

// knownTemplateIconHashes 是 Flutter 脚手架默认图标的像素指纹（SHA-256）。
//
// 为什么不比文件哈希：aapt2 会在打包时重新压缩 PNG，同一张图在 APK 内外的
// 字节序列不同（实测 7963 → 7143 字节），只有像素数据一致。因此这里解码后
// 对像素取哈希。
//
// 去重口径是 NRGBA（非预乘 alpha），不是 RGBA：模板图标是调色板 PNG、含透明
// 像素，两种口径算出的哈希不同（实测前者 ecee7054…、后者 c01eb3ca…）。
// 这份常量与同样按非预乘口径取值的工具（PIL 的 convert('RGBA').tobytes()）
// 算出的结果对齐。
//
// 列表来自 Flutter 模板（civilian/gwy 替换前的那批）。它是一份**黑名单**：
// 命中说明图标没换过，未命中不代表图标一定是定制的 —— 其它 Flutter 版本
// 或其它脚手架可能有别的默认图。渠道实测的驳回理由正是「图标与安装后不一致」，
// 而模板图标是最常见的成因。
var knownTemplateIconHashes = map[string]string{
	"f8239b4787017a6a522c6b78999890a867d3d663c55885787561fd1678e7e393": "mdpi 48x48",
	"24a5ad70e62630b6c50fb666a4f18466f82697a836e4d96a62c6a09673c0a15f": "hdpi 72x72",
	"7145a743cd09b9a18c14c0982d6d87aee7200d76fd555360a69cc29eec7beba6": "xhdpi 96x96",
	"ad3a5ce4a66b8fca8cbea055fa0880e5aa9bed1132b38d1fdf058167107f1dd0": "xxhdpi 144x144",
	"c01eb3ca97f32ac85d1c99cdbfc28041c9f529c707fe039d07b68b17e8f1d0fa": "xxxhdpi 192x192",
}

// IconCheck 是图标检查的结果。
type IconCheck struct {
	// Checked 表示是否真的读到了图标并算了指纹。
	// 图标是 XML（自适应图标）或资源引用未能展开时为 false —— 那是
	// 「无法判定」，而不是「通过」。
	Checked bool
	// IsTemplate 表示图标命中了已知的脚手架模板指纹。
	IsTemplate bool
	// Match 是命中的那个模板的描述（如 "xxxhdpi 192x192"）。
	Match string
	// Detail 说明为什么没能判定，供报告使用。
	Detail string
}

// CheckIcon 读取 APK 内声明的图标，判断它是否仍是脚手架模板图。
//
// 只检查 manifest 里 android:icon 解析出的那一张图，不做多密度遍历：手工替换
// 时各密度会一起换，抽查一张即可覆盖绝大多数漏改；而全量遍历在自适应图标
// 场景下会引入大量特例（XML 引用、背景层与前景层分离）。
func CheckIcon(apkPath string, iconPath string) IconCheck {
	entry := strings.TrimSpace(iconPath)
	if entry == "" {
		return IconCheck{Detail: "manifest 未声明 android:icon"}
	}
	// apkparser 把能展开的引用变成 res/xxx.png 这样的路径；
	// 展开失败时是 @0x7f... 或 @mipmap/...，此时无法定位文件
	if !strings.HasPrefix(entry, "res/") {
		return IconCheck{Detail: fmt.Sprintf("图标是资源引用（%s），未能定位到文件", entry)}
	}
	if !strings.HasSuffix(strings.ToLower(entry), ".png") {
		return IconCheck{
			Detail: fmt.Sprintf("图标 %s 不是 PNG（多半是自适应图标的 XML，属定制图标）", entry),
		}
	}

	data, err := readAPKEntry(apkPath, entry)
	if err != nil {
		return IconCheck{Detail: fmt.Sprintf("读取图标 %s 失败：%v", entry, err)}
	}
	hash, err := pixelHash(data)
	if err != nil {
		return IconCheck{Detail: fmt.Sprintf("解码图标 %s 失败：%v", entry, err)}
	}
	if desc, hit := knownTemplateIconHashes[hash]; hit {
		return IconCheck{Checked: true, IsTemplate: true, Match: desc}
	}
	return IconCheck{Checked: true}
}

// pixelHash 解码 PNG 并对像素取 SHA-256。
//
// 用 NRGBA（非预乘）而不是 RGBA：见 knownTemplateIconHashes 的说明。
func pixelHash(data []byte) (string, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	bounds := img.Bounds()
	nrgba := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			nrgba.Set(x, y, color.NRGBAModel.Convert(img.At(x, y)))
		}
	}
	sum := sha256.Sum256(nrgba.Pix)
	return hex.EncodeToString(sum[:]), nil
}

func readAPKEntry(path, entry string) ([]byte, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != entry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 32<<20))
	}
	return nil, os.ErrNotExist
}
