package harmony

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// API 是鸿蒙 AppGallery 的接口封装。无状态。
//
// 与华为渠道共用 token 端点、请求体、响应字段，因此复用同一套 tokenReq/tokenResp
// 语义；但发布流程完全不同：.app 走 Upload Management API 分片上传，
// 关联草稿用 v3 接口。
type API struct {
	client *http.Client
	// baseURL 可注入，测试里指向 httptest.Server
	baseURL string
}

// NewAPI 构造接口封装。baseURL 为空时用生产域名。
func NewAPI(client *http.Client, baseURL string) *API {
	if baseURL == "" {
		baseURL = BaseURL
	}
	if client == nil {
		client = httpx.Client(httpx.Default())
	}
	return &API{
		client:  client,
		baseURL: strings.TrimSuffix(baseURL, "/") + "/",
	}
}

// GetToken 取 access_token。
//
// 与华为渠道同源：同端点、同请求体、同响应字段。取 token 成功时华为不返回 ret，
// 因此这里只判 access_token 是否存在。
func (a *API) GetToken(ctx context.Context, clientID, clientSecret string) (string, error) {
	payload := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"grant_type":    "client_credentials",
	}
	body, err := a.postJSON(ctx, a.url(pathToken, nil), nil, payload)
	if err != nil {
		return "", err
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		Ret         *ret   `json:"ret"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return "", protocolError("获取token 的响应无法解析", body, err)
	}
	if resp.Ret != nil && resp.Ret.Code != nil && *resp.Ret.Code != 0 {
		return "", resp.Ret.checkSuccess("获取token")
	}
	token := strings.TrimSpace(resp.AccessToken)
	if token == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindCredential,
			Channel: ID,
			Msg:     "华为未返回 access_token，请检查 client_id 与 client_secret 是否正确",
		}
	}
	return token, nil
}

// GetAppInfoV3 查询鸿蒙应用的状态与审核意见。
//
// # 为什么用 v3 而不是复用 Android 版的 v2
//
// HarmonyOS NEXT 应用在 AGC 里是**独立的应用记录**，与同名 Android 应用不是同一个
// appId。用 Android 版的 app-info 查到的是另一个应用的记录 —— 本渠道曾因此直接声明
// 「不支持查询市场状态」，但那个结论下错了：v3 就是鸿蒙专用接口。
//
// v3/app-info 是鸿蒙专用的版本，用我们本来就要用户配置的 app_id 查询，
// 返回的 releaseState 与 auditInfo 都是这个鸿蒙应用的。
func (a *API) GetAppInfoV3(
	ctx context.Context,
	auth authHeader,
	appID string,
) (AppInfoV3, *AuditInfoV3, error) {
	req, err := a.newRequest(ctx, http.MethodGet,
		a.url(pathAppInfoV3, map[string]string{"appId": appID}), auth, nil)
	if err != nil {
		return AppInfoV3{}, nil, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return AppInfoV3{}, nil, err
	}
	resp, err := unmarshal[AppInfoRespV3](body, "查询应用信息")
	if err != nil {
		return AppInfoV3{}, nil, err
	}
	if err := resp.Ret.checkSuccess("查询应用信息"); err != nil {
		return AppInfoV3{}, nil, err
	}
	if resp.AppInfo == nil {
		return AppInfoV3{}, nil, protocolError(
			"华为未返回 appInfo，无法判断应用状态。请确认 app_id 是鸿蒙应用的 ID"+
				"（与同名 Android 应用不是同一个）", body, nil)
	}
	return *resp.AppInfo, resp.AuditInfo, nil
}

// InitMultipart 初始化分片上传，返回 objectId、nspUploadId 与分片大小。
func (a *API) InitMultipart(
	ctx context.Context,
	auth authHeader,
	appID, fileName string,
) (MultipartInitResp, error) {
	query := map[string]string{
		"appId":       appID,
		"fileName":    fileName,
		"contentType": InitContentType,
		"fileType":    fmt.Sprint(InitFileType),
		"releaseType": fmt.Sprint(InitReleaseType),
	}
	req, err := a.newRequest(ctx, http.MethodPost, a.url(pathMultipartInit, query), auth, nil)
	if err != nil {
		return MultipartInitResp{}, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return MultipartInitResp{}, err
	}
	resp, err := unmarshal[MultipartInitResp](body, "初始化分片上传")
	if err != nil {
		return MultipartInitResp{}, err
	}
	if err := resp.Ret.checkSuccess("初始化分片上传"); err != nil {
		return MultipartInitResp{}, err
	}
	if strings.TrimSpace(resp.ObjectID) == "" {
		return MultipartInitResp{}, protocolError("华为未返回 objectId，无法继续分片上传", body, nil)
	}
	if strings.TrimSpace(resp.NspUploadID) == "" {
		return MultipartInitResp{}, protocolError("华为未返回 nspUploadId，无法继续分片上传", body, nil)
	}
	return resp, nil
}

// GetPartUploadInfo 提交各分片的 sha256 与长度，换取每片的签名上传地址。
func (a *API) GetPartUploadInfo(
	ctx context.Context,
	auth authHeader,
	objectID, nspUploadID string,
	descriptors map[string]PartDescriptor,
) (MultipartPartsResp, error) {
	query := map[string]string{"objectId": objectID, "nspUploadId": nspUploadID}
	req, err := a.newJSONRequest(ctx, http.MethodPost, a.url(pathMultipartParts, query),
		auth, descriptors)
	if err != nil {
		return MultipartPartsResp{}, err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return MultipartPartsResp{}, err
	}
	resp, err := unmarshal[MultipartPartsResp](body, "获取分片上传地址")
	if err != nil {
		return MultipartPartsResp{}, err
	}
	if err := resp.Ret.checkSuccess("获取分片上传地址"); err != nil {
		return MultipartPartsResp{}, err
	}
	if len(resp.UploadInfoMap) == 0 {
		return MultipartPartsResp{}, protocolError("华为未返回 uploadInfoMap", body, nil)
	}
	return resp, nil
}

// Compose 合并分片。
func (a *API) Compose(
	ctx context.Context,
	auth authHeader,
	objectID, nspUploadID string,
	completed map[string]CompletedPart,
) error {
	query := map[string]string{"objectId": objectID, "nspUploadId": nspUploadID}
	req, err := a.newJSONRequest(ctx, http.MethodPost, a.url(pathMultipartCompose, query),
		auth, completed)
	if err != nil {
		return err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	resp, err := unmarshal[ComposeResp](body, "合并分片")
	if err != nil {
		return err
	}
	return resp.Ret.checkSuccess("合并分片")
}

// LinkDraft 把已合并的 App Pack 关联到当前草稿版本。
//
// 分片合并成功只代表文件进入了华为的文件服务，**必须调这个接口才会出现在
// AGC 后台的草稿里**。
func (a *API) LinkDraft(
	ctx context.Context,
	auth authHeader,
	appID, fileName, objectID string,
) (string, error) {
	payload := AppPackageInfoReq{FileName: fileName, ObjectID: objectID}
	req, err := a.newJSONRequest(ctx, http.MethodPut, a.url(pathAppPackageInfo,
		map[string]string{"appId": appID}), auth, payload)
	if err != nil {
		return "", err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return "", err
	}
	resp, err := unmarshal[AppPackageInfoResp](body, "关联 App Pack 到草稿")
	if err != nil {
		return "", err
	}
	if err := resp.Ret.checkSuccess("关联 App Pack 到草稿"); err != nil {
		return "", err
	}
	id := resp.resolvePackageID()
	if id == "" {
		return "", protocolError(
			"关联草稿成功但华为未返回 packageId，请到 AGC 后台确认草稿状态", body, nil)
	}
	return id, nil
}

// Submit 提交发布（送审）。
//
// 鸿蒙走 v3，Android 走 v2 —— 两者路径不同，不可混用。
// 这是整条链路里唯一不可撤销的一步。
func (a *API) Submit(ctx context.Context, auth authHeader, appID string, payload SubmitReq) error {
	req, err := a.newJSONRequest(ctx, http.MethodPost,
		a.url(pathAppSubmit, map[string]string{"appId": appID}), auth, payload)
	if err != nil {
		return err
	}
	body, err := httpx.Do(a.client, req, ID)
	if err != nil {
		return err
	}
	resp, err := unmarshal[SubmitResp](body, "提交发布")
	if err != nil {
		return err
	}
	return resp.Ret.checkSuccess("提交发布")
}

// UploadPart 上传单个分片，返回响应里的 ETag。
//
// 三个要点：
//   - 签名地址是短时效的，**必须原样转发华为给的请求头**，增删都可能导致签名失败
//   - 强制 https，避免分片内容与签名头明文外泄
//   - ETag 原样保留（含引号）回传给 compose，不做任何加工
func (a *API) UploadPart(
	ctx context.Context,
	partKey string,
	info PartUploadInfo,
	data []byte,
) (string, error) {
	rawURL := strings.TrimSpace(info.URL)
	if rawURL == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "分片 " + partKey + " 的上传地址缺失",
		}
	}
	if _, err := httpx.RequireHTTPS(rawURL, ID); err != nil {
		return "", err
	}
	method, err := normalizeMethod(info.Method)
	if err != nil {
		return "", err
	}
	headers, err := normalizePartHeaders(info.Headers)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", eperr.ConfigurationError("构造分片上传请求失败：%v", err)
	}
	req.ContentLength = int64(len(data))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", eperr.NetworkError(ID, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &eperr.Error{
			Kind:    eperr.KindChannelRejected,
			Channel: ID,
			Code:    fmt.Sprint(resp.StatusCode),
			Msg:     fmt.Sprintf("分片 %s 上传失败：HTTP %d %s", partKey, resp.StatusCode, resp.Status),
		}
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		etag = resp.Header.Get("etag")
	}
	if etag == "" {
		return "", &eperr.Error{
			Kind:    eperr.KindProtocolMismatch,
			Channel: ID,
			Msg:     "分片 " + partKey + " 的上传响应缺少 ETag，无法合并分片",
		}
	}
	return etag, nil
}

// ---- 内部 ----

// authHeader 是华为系的鉴权信息：Authorization 与 client_id 两个头。
type authHeader struct {
	token    string
	clientID string
}

func (a authHeader) apply(req *http.Request) {
	if a.token != "" {
		req.Header.Set("Authorization", a.token)
	}
	if a.clientID != "" {
		req.Header.Set("client_id", a.clientID)
	}
}

func (a *API) newRequest(
	ctx context.Context,
	method, rawURL string,
	auth authHeader,
	body io.Reader,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, eperr.ConfigurationError("构造鸿蒙请求失败：%v", err)
	}
	auth.apply(req)
	return req, nil
}

func (a *API) newJSONRequest(
	ctx context.Context,
	method, rawURL string,
	auth authHeader,
	payload any,
) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, eperr.ConfigurationError("序列化鸿蒙请求体失败：%v", err)
	}
	req, err := a.newRequest(ctx, method, rawURL, auth, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (a *API) postJSON(
	ctx context.Context,
	rawURL string,
	auth *authHeader,
	payload any,
) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", eperr.ConfigurationError("序列化鸿蒙请求体失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return "", eperr.ConfigurationError("构造鸿蒙请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != nil {
		auth.apply(req)
	}
	return httpx.Do(a.client, req, ID)
}

func (a *API) url(path string, query map[string]string) string {
	u := a.baseURL + path
	if len(query) == 0 {
		return u
	}
	values := url.Values{}
	for k, v := range query {
		values.Set(k, v)
	}
	return u + "?" + values.Encode()
}

func unmarshal[T any](body, action string) (T, error) {
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return out, protocolError(action+" 的响应无法解析，渠道接口可能已变更："+err.Error(), body, err)
	}
	return out, nil
}

func protocolError(msg, body string, cause error) error {
	return &eperr.Error{
		Kind:    eperr.KindProtocolMismatch,
		Channel: ID,
		Msg:     msg,
		Raw:     truncate(body),
		Err:     cause,
	}
}

// ---- 文件读取 ----

// readPartAt 读取文件在 [offset, offset+length) 区间的字节。
//
// 按偏移读取而不是流式顺序读：分片要先算摘要、再按华为返回的顺序上传，
// 两次遍历都必须从指定位置开始。
func readPartAt(f *os.File, offset, length int64) ([]byte, error) {
	if length <= 0 {
		return []byte{}, nil
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, eperr.LocalFileError("读取分片失败（offset=%d length=%d）：%v", offset, length, err)
	}
	return buf, nil
}
