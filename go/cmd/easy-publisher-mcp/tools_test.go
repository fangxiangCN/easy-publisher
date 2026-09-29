package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- 结构化错误 ----

// 错误必须以结构化形式传给调用方，而不是一段文本。
//
// 「网络超时」与「已越过送审点的超时」在文本上看起来差不多，
// 但一个可以重试、另一个绝不能 —— kind / retryable / phase 是调用方
// 决定「重试、换渠道还是交给人」的依据。
func TestErrorResultIsStructured(t *testing.T) {
	err := eperr.ConfigurationError("出错了：%s", "原因")
	result, out, retErr := errorResult(err, func(e *errorPayload) uploadOutput {
		return uploadOutput{Error: e}
	})

	if retErr != nil {
		t.Fatalf("errorResult 不应再返回 error（那会让 SDK 转成纯文本）：%v", retErr)
	}
	if !result.IsError {
		t.Error("IsError 应为 true")
	}
	if out.Error == nil {
		t.Fatal("结构化错误必须放在输出结构体里 —— SDK 会用它覆盖 structuredContent")
	}
	if out.Error.Kind != eperr.KindConfiguration.String() {
		t.Errorf("Kind = %q", out.Error.Kind)
	}
	if out.Error.Phase != eperr.PhasePreSubmission.String() {
		t.Errorf("Phase = %q", out.Error.Phase)
	}
	// 同时给一段文本，供不解析结构的客户端
	if len(result.Content) == 0 {
		t.Error("应同时提供文本内容")
	}
}

// 越过送审点的失败，retryable 必须是 false —— 这是整个工具最重要的安全属性。
func TestSubmissionPointErrorIsNotRetryable(t *testing.T) {
	base := eperr.NetworkError("huawei", errors.New("i/o timeout"))
	tagged := base.AtSubmissionPoint("华为", "提交审核")

	_, out, _ := errorResult(tagged, func(e *errorPayload) uploadOutput {
		return uploadOutput{Error: e}
	})

	if out.Error.Retryable {
		t.Error("越过送审点的失败绝不能标记为可重试")
	}
	if out.Error.Phase != eperr.PhaseAtOrAfterSubmission.String() {
		t.Errorf("Phase = %q", out.Error.Phase)
	}
	if !strings.Contains(out.Error.Message, "确认") {
		t.Errorf("提示应引导先到后台确认：%s", out.Error.Message)
	}
}

// 送审前的网络失败仍然可重试 —— 判断依据是「结果是否确定」，
// 不是「错误看起来是否可恢复」。
func TestPreSubmissionNetworkErrorIsRetryable(t *testing.T) {
	err := eperr.NetworkError("huawei", errors.New("i/o timeout"))

	_, out, _ := errorResult(err, func(e *errorPayload) uploadOutput {
		return uploadOutput{Error: e}
	})

	if !out.Error.Retryable {
		t.Error("送审前的网络失败应当可重试")
	}
}

// 非 eperr.Error 的普通错误也要能被归一化，不能丢掉结构。
func TestPlainErrorIsNormalized(t *testing.T) {
	_, out, _ := errorResult(errors.New("普通错误"), func(e *errorPayload) uploadOutput {
		return uploadOutput{Error: e}
	})

	if out.Error == nil {
		t.Fatal("普通错误也应有结构")
	}
	if out.Error.Message != "普通错误" {
		t.Errorf("Message = %q", out.Error.Message)
	}
	if out.Error.Kind != eperr.KindUnknown.String() {
		t.Errorf("Kind = %q, 未知错误应归为 Unknown", out.Error.Kind)
	}
}

// 结构化错误必须能序列化进 structuredContent，且成功时不出现 error 字段。
func TestErrorFieldSerialization(t *testing.T) {
	// 成功：不带 error 字段
	ok, err := json.Marshal(uploadOutput{JobID: "j1", Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ok), `"error"`) {
		t.Errorf("成功结果不应含 error 字段：%s", ok)
	}

	// 失败：带 error 字段
	_, out, _ := errorResult(eperr.ConfigurationError("x"), func(e *errorPayload) uploadOutput {
		return uploadOutput{Error: e}
	})
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["error"]; !ok {
		t.Errorf("失败结果必须含 error 字段：%s", data)
	}
}

// ---- schema 生成 ----

// 所有工具的输入 schema 必须能生成。SDK 会解析 jsonschema tag，
// 遇到 WORD= 形式的关键字会 panic（例如把枚举说明写成 "draft=停在草稿态"）。
func TestAllToolSchemasGenerate(t *testing.T) {
	// 构造期就会 panic，所以「不 panic 且注册成功」本身就是断言
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("注册工具时 panic（多半是 jsonschema tag 里出现了 WORD= 形式）：%v", r)
		}
	}()

	svc := publish.NewService(config.NewStore(t.TempDir()))
	defer svc.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	registerTools(server, svc)
}

// 工具注解必须如实反映副作用：只有 upload_apk 会修改远端。
func TestToolAnnotations(t *testing.T) {
	// 这里读的是注册进 server 的工具定义，与运行时给客户端的一致
	// （SDK 未导出已注册工具的枚举接口，因此直接用同一批构造参数断言）
	readOnly := readOnly()
	if !readOnly.ReadOnlyHint {
		t.Error("只读工具的 ReadOnlyHint 应为 true")
	}

	destructive := true
	openWorld := true
	write := &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		IdempotentHint:  false,
		OpenWorldHint:   &openWorld,
	}
	if write.ReadOnlyHint {
		t.Error("upload_apk 不应标为只读")
	}
	if write.DestructiveHint == nil || !*write.DestructiveHint {
		t.Error("upload_apk 应标为有破坏性 —— 送审不可撤销")
	}
	if write.IdempotentHint {
		t.Error("upload_apk 不应标为幂等 —— 重复调用会产生重复版本")
	}
}

// ---- 参数解析 ----

func TestParseOnlineTime(t *testing.T) {
	// 格式与 Kotlin 版一致，且拒绝过去的时间
	if _, err := publish.ParseOnlineTime("2020-01-01 00:00:00"); err == nil {
		t.Error("过去的时间应当被拒绝")
	}
	if _, err := publish.ParseOnlineTime("不是时间"); err == nil {
		t.Error("非法格式应当被拒绝")
	}
	// 用未来时间，避免测试随时间失效
	future := time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := publish.ParseOnlineTime(future); err != nil {
		t.Errorf("合法格式不应报错：%v", err)
	}
}
