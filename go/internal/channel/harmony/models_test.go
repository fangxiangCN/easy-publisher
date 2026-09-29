package harmony

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
)

// ---- 分片切分（纯函数，脱离网络验证）----

func TestPlanParts(t *testing.T) {
	cases := []struct {
		name     string
		fileSize int64
		partSize int64
		want     []partPlan
	}{
		{
			name:     "整除",
			fileSize: 300,
			partSize: 100,
			want: []partPlan{
				{Index: 1, Offset: 0, Length: 100},
				{Index: 2, Offset: 100, Length: 100},
				{Index: 3, Offset: 200, Length: 100},
			},
		},
		{
			name:     "有余数",
			fileSize: 250,
			partSize: 100,
			want: []partPlan{
				{Index: 1, Offset: 0, Length: 100},
				{Index: 2, Offset: 100, Length: 100},
				{Index: 3, Offset: 200, Length: 50},
			},
		},
		{
			name:     "不足一片",
			fileSize: 30,
			partSize: 100,
			want:     []partPlan{{Index: 1, Offset: 0, Length: 30}},
		},
		{
			name:     "恰好一片",
			fileSize: 100,
			partSize: 100,
			want:     []partPlan{{Index: 1, Offset: 0, Length: 100}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planParts(tc.fileSize, tc.partSize)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("片数 = %d, 期望 %d：%+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("第 %d 片 = %+v, 期望 %+v", i+1, got[i], tc.want[i])
				}
			}
			// 各片长度之和必须等于文件大小，否则 compose 会失败
			var total int64
			for _, p := range got {
				total += p.Length
			}
			if total != tc.fileSize {
				t.Errorf("各片长度之和 = %d, 期望 %d", total, tc.fileSize)
			}
		})
	}
}

func TestPlanPartsRejectsEmptyFile(t *testing.T) {
	if _, err := planParts(0, 100); err == nil {
		t.Error("空文件应当被拒绝")
	}
}

func TestPlanPartsFallsBackWhenPartSizeMissing(t *testing.T) {
	// 华为未返回 nspPartMinSize 时用兜底值，而不是报错或切出一片
	got, err := planParts(FallbackPartSize*2+10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("片数 = %d, 期望 3（应按兜底分片大小切）", len(got))
	}
}

func TestPartKeyNaming(t *testing.T) {
	// 键名是 Swagger 占位符风格，看着像没改完的代码，但必须逐字匹配 ——
	// 改成 "part1" 之类会被服务端拒绝
	p := partPlan{Index: 3}
	if got := p.key(); got != "additionalProp3" {
		t.Errorf("key() = %q, 期望 additionalProp3", got)
	}
	if PartKeyPrefix != "additionalProp" {
		t.Errorf("PartKeyPrefix = %q", PartKeyPrefix)
	}
}

// ---- 请求头归一化 ----

func TestNormalizePartHeaders(t *testing.T) {
	t.Run("JSON 对象", func(t *testing.T) {
		// encoding/json 把对象解成 map[string]any
		got, err := normalizePartHeaders(map[string]any{
			"Content-Type": "application/octet-stream",
			"x-obs-auth":   "sig",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got["Content-Type"] != "application/octet-stream" || got["x-obs-auth"] != "sig" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("JSON 字符串", func(t *testing.T) {
		// 华为也可能返回序列化后的字符串。两种形状都要能处理，
		// 否则会因为一个字段形状变化就让整次上传失败
		got, err := normalizePartHeaders(`{"Content-Type":"text/plain","x-a":"1"}`)
		if err != nil {
			t.Fatal(err)
		}
		if got["Content-Type"] != "text/plain" || got["x-a"] != "1" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("空值", func(t *testing.T) {
		for _, in := range []any{nil, "", "  "} {
			got, err := normalizePartHeaders(in)
			if err != nil {
				t.Errorf("%v 不应报错：%v", in, err)
			}
			if len(got) != 0 {
				t.Errorf("%v 应得到空表，实际 %+v", in, got)
			}
		}
	})

	t.Run("非法 JSON 字符串", func(t *testing.T) {
		// 不能静默返回空表 —— 那会让后续上传缺少必要的签名头，
		// 报错信息却指向别处
		if _, err := normalizePartHeaders("not json"); err == nil {
			t.Error("非法 JSON 应当报错")
		}
	})

	t.Run("非字符串值转成文本", func(t *testing.T) {
		got, err := normalizePartHeaders(map[string]any{"x-num": 42, "x-bool": true})
		if err != nil {
			t.Fatal(err)
		}
		if got["x-num"] != "42" || got["x-bool"] != "true" {
			t.Errorf("got %+v", got)
		}
	})
}

func TestNormalizeMethod(t *testing.T) {
	cases := map[string]string{
		"":       "PUT", // 华为未指定时默认 PUT
		"put":    "PUT",
		"POST":   "POST",
		" post ": "POST",
	}
	for in, want := range cases {
		got, err := normalizeMethod(in)
		if err != nil {
			t.Errorf("%q 不应报错：%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeMethod(%q) = %q, 期望 %q", in, got, want)
		}
	}
	// 只接受 PUT 与 POST
	for _, bad := range []string{"DELETE", "PATCH", "GET"} {
		if _, err := normalizeMethod(bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

// ---- 提审备注 ----

func TestBuildRemark(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantNil bool
	}{
		{"空字符串不传", "", true},
		{"纯空白不传", "   \n  ", true},
		{"太短不传", "太短了", true},
		{"合规长度传", "这是一个足够长的更新说明用于满足十字要求", false},
		{"恰好 10 字传", "一二三四五六七八九十", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := buildRemark(tc.input)
			if tc.wantNil {
				if got != nil {
					t.Errorf("期望不传，实际传了 %q", *got)
				}
				// 跳过「长度不合规」时要给出原因，否则用户不知道更新说明为什么没生效。
				// 但输入本身为空或纯空白等同于没传，不需要说明
				trimmed := strings.TrimSpace(tc.input)
				if trimmed != "" && reason == "" {
					t.Error("因长度不合规跳过时应说明原因")
				}
				return
			}
			if got == nil {
				t.Fatalf("期望传 remark，实际跳过了（原因：%s）", reason)
			}
			if *got != tc.input {
				t.Errorf("remark = %q, 期望 %q", *got, tc.input)
			}
		})
	}
}

func TestBuildRemarkCountsRunesNotBytes(t *testing.T) {
	// 华为按字数限制，不是字节数。中文一个字 3 字节，
	// 若按字节判断，「一二三四五六七八九十」会被误判为超长
	got, _ := buildRemark("一二三四五六七八九十")
	if got == nil {
		t.Error("10 个中文字应当合规，不应按字节数误判")
	}

	// 300 个中文字正好在上限
	at300 := ""
	for i := 0; i < RemarkMax; i++ {
		at300 += "中"
	}
	if got, reason := buildRemark(at300); got == nil {
		t.Errorf("%d 个中文字应当合规，实际跳过：%s", RemarkMax, reason)
	}

	// 301 个超限
	if got, _ := buildRemark(at300 + "中"); got != nil {
		t.Error("超过上限应当跳过")
	}
}

// ---- 响应解析 ----

func TestAppPackageInfoResolvePackageID(t *testing.T) {
	// packageId 可能直接在顶层，也可能嵌在 data 里 —— 两种形状都要接住
	var topLevel AppPackageInfoResp
	if err := json.Unmarshal([]byte(`{"ret":{"code":0},"packageId":"pkg-top"}`), &topLevel); err != nil {
		t.Fatal(err)
	}
	if got := topLevel.resolvePackageID(); got != "pkg-top" {
		t.Errorf("顶层 packageId 解析为 %q", got)
	}

	var nested AppPackageInfoResp
	if err := json.Unmarshal([]byte(`{"ret":{"code":0},"data":{"packageId":"pkg-nested"}}`), &nested); err != nil {
		t.Fatal(err)
	}
	if got := nested.resolvePackageID(); got != "pkg-nested" {
		t.Errorf("data.packageId 解析为 %q", got)
	}

	var none AppPackageInfoResp
	if err := json.Unmarshal([]byte(`{"ret":{"code":0}}`), &none); err != nil {
		t.Fatal(err)
	}
	if got := none.resolvePackageID(); got != "" {
		t.Errorf("都缺失时应当为空，实际 %q", got)
	}
}

func TestSubmitReqOmitsEmptyFields(t *testing.T) {
	// 不传 releaseType / releasePhase 即全网发布。
	// 空字段必须从 JSON 里省略，而不是传 null
	data, err := json.Marshal(SubmitReq{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("空 SubmitReq 序列化为 %s, 期望 {}", data)
	}

	remark := "这是一条合规的提审备注内容"
	time := "2026-01-15T10:00:00+0800"
	idType := 1
	data, err = json.Marshal(SubmitReq{
		Remark: &remark, ReleaseTime: &time,
		RegisteredIDType: &idType, RegisteredIDNumber: "12345",
	})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	// 键名与华为接口逐字一致：registeredIdNumber 的 d 是小写
	for _, key := range []string{"remark", "releaseTime", "registeredIdType", "registeredIdNumber"} {
		if _, ok := back[key]; !ok {
			t.Errorf("缺少字段 %s：%s", key, data)
		}
	}
}

func TestRetCheckSuccess(t *testing.T) {
	t.Run("code 为 0 是成功", func(t *testing.T) {
		code := 0
		if err := (&ret{Code: &code}).checkSuccess("测试"); err != nil {
			t.Errorf("不应报错：%v", err)
		}
	})
	t.Run("code 缺失视为成功", func(t *testing.T) {
		if err := (&ret{}).checkSuccess("测试"); err != nil {
			t.Errorf("code 缺失不应报错：%v", err)
		}
	})
	t.Run("ret 整体缺失是协议错误", func(t *testing.T) {
		var r *ret
		if err := r.checkSuccess("测试"); err == nil {
			t.Error("ret 缺失应当报错而不是当成成功")
		}
	})
	t.Run("非 0 带上错误码", func(t *testing.T) {
		code := 204144660
		err := (&ret{Code: &code, Msg: "submit failed"}).checkSuccess("提交发布")
		if err == nil {
			t.Fatal("应当报错")
		}
		// 错误码要出现在 message 里：用户拿它去查华为文档
		if !strings.Contains(err.Error(), "code=204144660") {
			t.Errorf("message 应带上错误码：%v", err)
		}
		if !strings.Contains(err.Error(), "submit failed") {
			t.Errorf("应保留渠道的原始描述：%v", err)
		}
	})
}

// ---- 制品类型 ----

func TestOnlyAcceptsAppPack(t *testing.T) {
	kinds := New().ArtifactKinds()
	if len(kinds) != 1 || kinds[0] != artifact.KindHarmonyAppPack {
		t.Errorf("ArtifactKinds = %v, 鸿蒙只应接受 App Pack", kinds)
	}
}

func TestUploadRejectsNonAppPack(t *testing.T) {
	// 把 .apk 传给鸿蒙渠道必须立刻报错，而不是等到上传几兆之后。
	// 注意这个校验在 RequireSupportedStage 之后、任何网络调用之前
	_, err := New().Upload(context.Background(), channel.UploadRequest{
		ArtifactInfo: artifact.Info{
			ApplicationID: "com.example.app",
			Kind:          artifact.KindAPK,
		},
		Credentials: channel.NewCredentials(nil),
		StopAfter:   channel.StageSubmitReview,
	})
	if err == nil {
		t.Fatal("非 App Pack 应当被拒绝")
	}
	if !strings.Contains(err.Error(), ".app") {
		t.Errorf("应说明需要什么格式：%v", err)
	}
}
