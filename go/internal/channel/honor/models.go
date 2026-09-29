// Package honor 实现荣耀应用市场渠道。
package honor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/jsonx"
)

// ID 是渠道标识。
const ID = "honor"

const (
	// BaseURL 是业务接口的域名
	BaseURL = "https://appmarket-openapi-drcn.cloud.honor.com/"
	// TokenURL 是 IAM 域名，与业务接口不同域
	TokenURL = "https://iam.developer.honor.com/auth/token"

	// APKFileType 是荣耀约定的 APK 安装包类型。保持原值 100，不要改成其他数字。
	APKFileType = 100
	// APKMediaType 是上传时的 Content-Type
	APKMediaType = "application/vnd.android.package-archive"

	maxRaw = 2000
	logTag = "荣耀应用市场"
)

// 各接口路径
const (
	pathGetAppID          = "openapi/v1/publish/get-app-id"
	pathGetAppDetail      = "openapi/v1/publish/get-app-detail"
	pathGetCurrentRelease = "openapi/v1/publish/get-app-current-release"
	pathGetUploadURL      = "openapi/v1/publish/get-file-upload-url"
	pathUpdateFileInfo    = "openapi/v1/publish/update-file-info"
	pathUpdateLanguage    = "openapi/v1/publish/update-language-info"
	pathSubmitAudit       = "openapi/v1/publish/submit-audit"
	pathUpdateAppInfo     = "openapi/v1/publish/update-app-info"
)

// result 是荣耀的通用响应包装。
//
// 全部字段可空：渠道随时可能增删字段，用非空字段声明会让反序列化在字段缺失时
// 直接失败，而错误信息里只有字段名，排查不到是哪一步。缺失的必要字段改为在
// 业务层显式校验，抛带中文说明的错误。
type result[T any] struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
	Data *T     `json:"data"`
}

// checkSuccess 判定业务码。荣耀以 code == 0 表示成功。
func (r result[T]) checkSuccess(action string) error {
	if r.Code != nil && *r.Code == 0 {
		return nil
	}
	code := ""
	if r.Code != nil {
		code = fmt.Sprint(*r.Code)
	}
	msg := strings.TrimSpace(r.Msg)
	if msg == "" {
		msg = "渠道未返回错误描述"
	}
	// 错误码要出现在给人看的 message 里：用户拿它去查渠道文档，
	// 只给一句自然语言描述等于把唯一的线索藏起来
	if code != "" {
		msg = fmt.Sprintf("%s (code=%s)", msg, code)
	}
	return &eperr.Error{
		Kind:    eperr.KindChannelRejected,
		Channel: ID,
		Code:    code,
		Msg:     fmt.Sprintf("%s 失败：%s", action, msg),
	}
}

// requireData 校验成功并取出 data。
//
// data 为空时把动作名带进错误信息，避免只剩一句没有上下文的解析失败。
func (r result[T]) requireData(action string) (T, error) {
	var zero T
	if err := r.checkSuccess(action); err != nil {
		return zero, err
	}
	if r.Data == nil {
		return zero, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 成功但未返回数据，渠道接口可能已变更",
		}
	}
	return *r.Data, nil
}

// tokenResp 是 token 接口的响应。
type tokenResp struct {
	Token string `json:"access_token"`
}

// appIDEntry 是 get-app-id 的返回项。
type appIDEntry struct {
	PackageName string `json:"packageName"`
	// appId 在荣耀文档里写作字符串，实际返回裸数字（如 900876322），
	// 用 FlexString 同时接受两种形态。
	AppID jsonx.FlexString `json:"appId"`
}

// AppInfo 是应用详情。
type AppInfo struct {
	LanguageInfo []LanguageInfo  `json:"languageInfo"`
	ReleaseInfo  *PubReleaseInfo `json:"releaseInfo"`
	BasicInfo    *BasicInfo      `json:"basicInfo"`
}

// BasicInfo 是应用的基础信息。
//
// 其中 RatingId 是**年龄分级**（3+ / 8+ / 12+ 等，取值见荣耀的年龄分级标准）。
// 它会阻塞送审：submit-audit 要求该字段非空，为空时报
// `app rating id is empty (code=20046)` —— 而提示里完全没说是哪个字段、
// 也没说去哪设置，只能靠 get-app-detail 的返回比对出来。
//
// 其余字段是 update-app-info 的必填项。该接口是全量更新语义，
// 改一个字段也要把整份资料回传，因此全部保留。
type BasicInfo struct {
	AppCategoryId     *int   `json:"appCategoryId"`
	AppClassification string `json:"appClassification"`
	SupplyName        string `json:"supplyName"`
	SupplyNameEn      string `json:"supplyNameEn"`
	DevName           string `json:"devName"`
	DevNameEn         string `json:"devNameEn"`
	DefaultLanguage   string `json:"defaultLanguage"`
	ReleaseCountry    string `json:"releaseCountry"`
	GameType          *int   `json:"gameType"`
	PaymentInfo       *int   `json:"paymentInfo"`
	PrivacyPolicyUrl  string `json:"privacyPolicyUrl"`
	// RatingId 为 nil 表示尚未设置年龄分级，会导致送审被拒
	RatingId *int `json:"ratingId"`
}

// LanguageInfo 是语言信息。改更新说明时要回填其中的 appName / intro。
type LanguageInfo struct {
	LanguageID string `json:"languageId"`
	AppName    string `json:"appName"`
	Intro      string `json:"intro"`
	BriefIntro string `json:"briefIntro"`
}

// PubReleaseInfo 是线上版本信息。
type PubReleaseInfo struct {
	VersionCode *jsonx.FlexInt64 `json:"versionCode"`
	VersionName string           `json:"versionName"`
}

// uploadFile 是申请上传地址时的文件描述。
type uploadFile struct {
	FileName   string `json:"fileName"`
	FileType   int    `json:"fileType"`
	FileSize   int64  `json:"fileSize"`
	FileSha256 string `json:"fileSha256"`
}

// UploadURL 是上传地址与 objectId。objectId 在绑定文件时必须回传。
type UploadURL struct {
	URL      string `json:"uploadUrl"`
	ObjectID *int64 `json:"objectId"`
}

// uploadAck 是上传接口的响应。
//
// 上传接口返回**裸 JSON**，不是 result 包装体，但同样以 code == 0 表示成功。
// 这是一个容易踩的点：套用通用的 result 解析会拿不到 code。
type uploadAck struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
}

// bindApkFile 是绑定文件的请求体。
type bindApkFile struct {
	Items []bindItem `json:"bindingFileList"`
}

type bindItem struct {
	ObjectID int64 `json:"objectId"`
}

// versionDesc 是更新说明的请求体。
type versionDesc struct {
	List []versionDescItem `json:"languageInfoList"`
}

type versionDescItem struct {
	AppName    string  `json:"appName"`
	Intro      string  `json:"intro"`
	BriefIntro *string `json:"briefIntro"`
	NewFeature string  `json:"newFeature"`
	LanguageID string  `json:"languageId"`
}

// submitParam 是提交审核的请求体。
type submitParam struct {
	// ReleaseType：1 全网发布，2 指定时间发布
	ReleaseType int `json:"releaseType"`
	// ReleaseTime 仅「指定时间发布」必填，格式 yyyy-MM-dd'T'HH:mm:ssZZ
	ReleaseTime *string `json:"releaseTime"`
}

// ReviewState 是审核状态。
type ReviewState struct {
	// AuditResult 取值：
	//
	//	0 审核中 / 1 审核通过 / 2 审核不通过 / 3 其他非审核状态 / 4 编辑中未提审
	AuditResult *int             `json:"auditResult"`
	VersionCode *jsonx.FlexInt64 `json:"versionCode"`
	VersionName string           `json:"versionName"`
	// AuditMessage 是审核意见。官方文档标为「否」（非必填），
	// 但响应示例里给出了实例值，如 "审核通过：XXX"。
	//
	// 注意它不一定只在被拒时出现 —— 示例里审核通过（auditResult=1）也带了内容。
	AuditMessage *jsonx.FlexString `json:"auditMessage"`
	// AuditAttachment 是审核意见附件的 URL，审核员截的图常在这里。
	// 官方文档：「审核意见附件，为url，可查看或下载」
	AuditAttachment []string `json:"auditAttachment"`
}

// ToMarketInfo 转成渠道无关的状态。
//
// 版本信息缺失时 LastVersion 传 nil 而不是伪造 0 —— 商店里只有草稿版本时荣耀
// 不返回版本号，上层「线上版本」展示与版本号比较都需要能区分「没有」和「是 0」。
func (r ReviewState) ToMarketInfo() channel.MarketInfo {
	state := channel.ReviewUnknown
	raw := "auditResult=null"
	if r.AuditResult != nil {
		raw = fmt.Sprintf("auditResult=%d", *r.AuditResult)
		switch *r.AuditResult {
		case 0:
			state = channel.ReviewUnderReview
		case 1:
			state = channel.ReviewOnline
		case 2:
			state = channel.ReviewRejected
		case 3:
			// 3 是「其他非审核状态」，荣耀文档未细分，无法判断是下架还是别的，
			// 归为未知而不是猜成 Offline
			state = channel.ReviewUnknown
		case 4:
			state = channel.ReviewDraft
		}
	}

	var version *channel.Version
	if r.VersionCode != nil {
		version = &channel.Version{Code: int64(*r.VersionCode), Name: r.VersionName}
	}

	info := channel.NewMarketInfo(ID, state, version, raw)
	feedback := &channel.ReviewFeedback{}
	if r.AuditMessage != nil {
		feedback.Opinion = strings.TrimSpace(string(*r.AuditMessage))
	}
	for _, url := range r.AuditAttachment {
		if trimmed := strings.TrimSpace(url); trimmed != "" {
			feedback.Attachments = append(feedback.Attachments, trimmed)
		}
	}
	// 渠道返回了结构但内容为空时不留空壳，让调用方用 info.Review != nil 判断即可
	if !feedback.Empty() {
		info.Review = feedback
	}
	return info
}

// fileSHA256 计算文件的 SHA-256。
//
// 荣耀在申请上传地址时必须提交该值，服务端会校验。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", eperr.LocalFileError("无法打开文件以计算 SHA-256：%s（%v）", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", eperr.LocalFileError("计算文件 SHA-256 失败：%s（%v）", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func truncate(s string) string {
	if len(s) <= maxRaw {
		return s
	}
	return s[:maxRaw] + "…"
}

// unmarshal 解析响应，失败时给带渠道与原始片段的协议错误。
func unmarshal[T any](body, action string) (T, error) {
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return out, &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     action + " 的响应无法解析，渠道接口可能已变更：" + err.Error(),
			Raw:     truncate(body),
			Err:     err,
		}
	}
	return out, nil
}
