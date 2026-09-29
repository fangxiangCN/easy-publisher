// Package artifact 解析待发布的制品（APK / HarmonyOS App Pack）元信息。
package artifact

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/avast/apkparser"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// Kind 是制品类型。决定怎么解析元信息，也决定能发给哪些渠道。
type Kind int

const (
	// KindAPK 是 Android 安装包
	KindAPK Kind = iota
	// KindHarmonyAppPack 是 HarmonyOS 5+ 的 App Pack。
	//
	// 与 APK 的差异不只是文件格式：它必须走华为的分片上传接口
	// （upload/multipart/init → parts → compose），再用 v3 的 app-package-info
	// 关联到草稿。普通的 upload-url + 单次 PUT 不适用于 .app。
	KindHarmonyAppPack
)

func (k Kind) String() string {
	if k == KindHarmonyAppPack {
		return "App Pack"
	}
	return "APK"
}

// Extensions 返回该类型接受的文件扩展名（不含点）。
func (k Kind) Extensions() []string {
	if k == KindHarmonyAppPack {
		return []string{"app"}
	}
	return []string{"apk"}
}

// KindOfExtension 按扩展名识别制品类型，无法识别时返回 ok=false。
func KindOfExtension(ext string) (Kind, bool) {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "apk":
		return KindAPK, true
	case "app":
		return KindHarmonyAppPack, true
	default:
		return KindAPK, false
	}
}

// KindOfFile 按文件扩展名识别。
func KindOfFile(path string) (Kind, bool) {
	return KindOfExtension(filepath.Ext(path))
}

// Info 是一个制品的元信息。
type Info struct {
	Path          string
	ApplicationID string
	VersionCode   int64
	VersionName   string
	SizeBytes     int64
	Kind          Kind
}

func (i Info) FileName() string { return filepath.Base(i.Path) }

func (i Info) String() string {
	return fmt.Sprintf("%s %s, versionCode=%d, versionName=%s",
		i.Kind, i.ApplicationID, i.VersionCode, i.VersionName)
}

// Read 按扩展名分派解析制品元信息。
func Read(path string) (Info, error) {
	st, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return Info{}, eperr.LocalFileError("制品文件不存在：%s", path)
	case err != nil:
		return Info{}, eperr.LocalFileError("无法读取制品文件 %s: %v", path, err)
	case st.IsDir():
		return Info{}, eperr.LocalFileError("不是文件：%s", path)
	case st.Size() == 0:
		return Info{}, eperr.LocalFileError("制品文件为空：%s", path)
	}

	kind, ok := KindOfFile(path)
	if !ok {
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		return Info{}, eperr.LocalFileError(
			"不支持的制品格式 .%s（%s）。支持：APK(.apk)、App Pack(.app)",
			ext, filepath.Base(path),
		)
	}

	base := Info{Path: path, SizeBytes: st.Size(), Kind: kind}
	switch kind {
	case KindHarmonyAppPack:
		return readAppPack(base)
	default:
		return readAPK(base)
	}
}

// ---- APK ----

// manifestScanner 实现 apkparser.ManifestEncoder，从二进制 AndroidManifest.xml 里
// 取出三个字段。
//
// apkparser 不提供 manifest struct，而是把解析出的 xml.Token 流交给调用方 ——
// 这样它不必为每种 manifest 结构建模，代价是使用方要自己扫。
// 我们只需要 <manifest> 根元素上的三个属性，所以拿到就返回 ErrEndParsing 提前结束，
// 不必解析整个文件（大 APK 的 manifest 可能有很多 activity/provider 声明）。
type manifestScanner struct {
	packageName string
	versionCode string
	versionName string
}

func (s *manifestScanner) EncodeToken(t xml.Token) error {
	se, ok := t.(xml.StartElement)
	if !ok || se.Name.Local != "manifest" {
		return nil
	}
	for _, a := range se.Attr {
		switch a.Name.Local {
		case "package":
			s.packageName = a.Value
		case "versionCode":
			s.versionCode = a.Value
		case "versionName":
			s.versionName = a.Value
		}
	}
	return apkparser.ErrEndParsing
}

func (s *manifestScanner) Flush() error { return nil }

func readAPK(base Info) (Info, error) {
	scanner := &manifestScanner{}
	// ParseApk 返回三个 error：zip 打开失败、resources.arsc 解析失败、manifest 解析失败。
	// 我们只需要 manifest，因此 resources 的错误可以忽略 —— 版本号不在资源表里。
	zipErr, _, manErr := apkparser.ParseApk(base.Path, scanner)
	if zipErr != nil {
		return Info{}, eperr.LocalFileError("打开 APK 失败：%s（%v）", base.Path, zipErr)
	}
	// ErrEndParsing 是我们主动结束解析的信号，不是错误
	if manErr != nil && manErr != apkparser.ErrEndParsing {
		return Info{}, eperr.LocalFileError("解析 APK 的 AndroidManifest 失败：%s（%v）", base.Path, manErr)
	}

	if scanner.packageName == "" {
		return Info{}, eperr.LocalFileError(
			"APK 的 AndroidManifest 里没有 package 属性：%s", base.Path)
	}
	code, err := strconv.ParseInt(scanner.versionCode, 10, 64)
	if err != nil {
		return Info{}, eperr.LocalFileError(
			"APK 的 versionCode 不是合法整数（%q）：%s", scanner.versionCode, base.Path)
	}

	base.ApplicationID = scanner.packageName
	base.VersionCode = code
	base.VersionName = scanner.versionName
	return base, nil
}

// ---- HarmonyOS App Pack ----

// packInfo 是 .app 包根目录下 pack.info 的结构。
//
// 全部字段用指针：HSP 的 pack.info 不含 summary/app，缺字段应该走到带说明的
// 校验分支，而不是解析出零值后被当成「版本号是 0」。
type packInfo struct {
	Summary *struct {
		App *struct {
			BundleName *string `json:"bundleName"`
			Version    *struct {
				Code *int64  `json:"code"`
				Name *string `json:"name"`
			} `json:"version"`
		} `json:"app"`
	} `json:"summary"`
}

const packInfoEntry = "pack.info"

// readAppPack 解析 HarmonyOS App Pack。
//
// .app 本质是 zip，元信息在根目录的 pack.info（JSON），结构为
// summary.app.{bundleName, version.{code, name}}。
//
// 这比 APK 简单：APK 的版本信息在二进制 AndroidManifest.xml 里、需要专门的解析器，
// 而 pack.info 是纯 JSON，标准库就够。
func readAppPack(base Info) (Info, error) {
	data, err := readZipEntry(base.Path, packInfoEntry)
	if err != nil {
		return Info{}, err
	}

	var pi packInfo
	if err := json.Unmarshal(data, &pi); err != nil {
		return Info{}, eperr.LocalFileError(
			"App Pack 的 %s 无法解析：%s（%v）", packInfoEntry, base.FileName(), err)
	}

	if pi.Summary == nil || pi.Summary.App == nil ||
		pi.Summary.App.BundleName == nil || *pi.Summary.App.BundleName == "" {
		return Info{}, eperr.LocalFileError(
			"App Pack 的 %s 缺少 summary.app.bundleName，无法确定包名：%s",
			packInfoEntry, base.FileName())
	}
	// 版本号缺失时不做静默降级：PublishPolicy 的版本比对依赖它，
	// 拿不到就明确报错，而不是跳过校验却让人以为已经检查过了
	if pi.Summary.App.Version == nil || pi.Summary.App.Version.Code == nil {
		return Info{}, eperr.LocalFileError(
			"App Pack 的 %s 缺少 summary.app.version.code，无法做版本号校验：%s",
			packInfoEntry, base.FileName())
	}

	base.ApplicationID = *pi.Summary.App.BundleName
	base.VersionCode = *pi.Summary.App.Version.Code
	if pi.Summary.App.Version.Name != nil && *pi.Summary.App.Version.Name != "" {
		base.VersionName = *pi.Summary.App.Version.Name
	} else {
		base.VersionName = strconv.FormatInt(base.VersionCode, 10)
	}
	return base, nil
}

func readZipEntry(path, entry string) ([]byte, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, eperr.LocalFileError("打开 App Pack 失败：%s（%v）", path, err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.Name != entry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, eperr.LocalFileError("读取 %s 失败：%s（%v）", entry, path, err)
		}
		defer rc.Close()
		// 限制读取大小：pack.info 正常只有几 KB，给个宽松上限防止构造的恶意包耗尽内存
		data, err := io.ReadAll(io.LimitReader(rc, maxPackInfoBytes))
		if err != nil {
			return nil, eperr.LocalFileError("读取 %s 失败：%s（%v）", entry, path, err)
		}
		return data, nil
	}
	return nil, eperr.LocalFileError(
		"App Pack 里找不到 %s：%s。请确认这是 DevEco Studio 打出的 .app 发布包，而不是单个 .hap",
		entry, filepath.Base(path))
}

const maxPackInfoBytes = 4 << 20

// ---- 包名校验 ----

var applicationIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// ValidateApplicationID 校验包名格式。
//
// 配置文件名直接由包名拼成，未过滤的 / 与 .. 会写到目录外。
// 鸿蒙的 bundleName 与 Android 包名规则一致（点分、至少两段）。
func ValidateApplicationID(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", eperr.ConfigurationError("包名不能为空")
	}
	if !applicationIDPattern.MatchString(trimmed) {
		return "", eperr.ConfigurationError(
			"包名格式不合法：%s（应形如 com.example.app）", trimmed)
	}
	return trimmed, nil
}

// IsValidApplicationID 报告包名是否合法，不返回错误。
func IsValidApplicationID(value string) bool {
	_, err := ValidateApplicationID(value)
	return err == nil
}
