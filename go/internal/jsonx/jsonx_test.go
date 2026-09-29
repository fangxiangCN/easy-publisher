package jsonx

import (
	"encoding/json"
	"testing"
)

// 这些用例的数据形状取自真实渠道响应，不是臆造的。
// 回归价值就在这里：测试 fixture 一旦用「理想类型」，问题就测不出来。

func TestFlexInt64AcceptsStringAndNumber(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		// 真实响应：OPPO app/info、vivo app.details 都返回字符串
		{"OPPO/vivo 真实形态", `"10"`, 10, false},
		// 真实响应：华为 app-info 返回数字
		{"华为真实形态", `1000`, 1000, false},
		{"带空白", `" 10 "`, 10, false},
		{"空字符串报错（与 Moshi 行为一致）", `""`, 0, true},
		{"null 不改动", `null`, 0, false},
		{"浮点截断", `10.9`, 10, false},
		{"字符串浮点", `"10.0"`, 10, false},
		{"大整数不丢精度", `"9008763221234567890"`, 9008763221234567890, false},
		{"非数字字符串报错", `"abc"`, 0, true},
		{"对象报错", `{}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got FlexInt64
			err := json.Unmarshal([]byte(c.in), &got)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，却解析成功: %d", int64(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", c.in, err)
			}
			if int64(got) != c.want {
				t.Errorf("得到 %d，期望 %d", int64(got), c.want)
			}
		})
	}
}

func TestFlexInt64PointerNilOnMissing(t *testing.T) {
	// 指针语义必须保持：字段缺失 → nil，而不是 0。
	// 这与类型兼容是两件事，不能混为一谈。
	type payload struct {
		Present *FlexInt64 `json:"present"`
		Missing *FlexInt64 `json:"missing"`
		Null    *FlexInt64 `json:"null"` //nolint:revive // 字段名与 JSON key 对应
	}
	var p payload
	if err := json.Unmarshal([]byte(`{"present":"10","null":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Present == nil || int64(*p.Present) != 10 {
		t.Errorf("present 应为 10，实际 %v", p.Present)
	}
	if p.Missing != nil {
		t.Errorf("missing 应为 nil，实际 %v", *p.Missing)
	}
	if p.Null != nil {
		t.Errorf("null 应为 nil，实际 %v", *p.Null)
	}
}

func TestFlexStringAcceptsNumberAndString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// 真实响应：荣耀 get-app-id 返回裸数字
		{"荣耀真实形态（数字）", `900876322`, "900876322"},
		{"字符串", `"900876322"`, "900876322"},
		{"空", `""`, ""},
		{"null", `null`, ""},
		{"大整数不丢精度", `9008763221234567890`, "9008763221234567890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got FlexString
			if err := json.Unmarshal([]byte(c.in), &got); err != nil {
				t.Fatalf("解析 %s 失败: %v", c.in, err)
			}
			if string(got) != c.want {
				t.Errorf("得到 %q，期望 %q", string(got), c.want)
			}
		})
	}
}

func TestFlexBoolVariants(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`true`, true}, {`false`, false},
		{`"true"`, true}, {`"false"`, false},
		{`"1"`, true}, {`"0"`, false},
		{`1`, true}, {`0`, false},
		{`null`, false}, {`""`, false},
	}
	for _, c := range cases {
		var got FlexBool
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Errorf("解析 %s 失败: %v", c.in, err)
			continue
		}
		if bool(got) != c.want {
			t.Errorf("解析 %s 得到 %v，期望 %v", c.in, bool(got), c.want)
		}
	}
}

// TestRealOppoResponseShape 用真实 OPPO 响应的关键片段做端到端解析。
// 这段 JSON 是从线上响应中截取的，字段顺序与类型都保持原样。
func TestRealOppoResponseShape(t *testing.T) {
	real := []byte(`{"errno":0,"data":{"app_id":"31379687","pkg_name":"tech.lightsoft.civilian",
		"app_name":"军队文职真题","version_id":"23823793","version_code":"10",
		"version_name":"2023.10.10","audit_status":111,"apk_size":"59958236"}}`)

	var resp struct {
		Errno *int `json:"errno"`
		Data  *struct {
			AppName     string     `json:"app_name"`
			VersionCode *FlexInt64 `json:"version_code"`
			VersionName string     `json:"version_name"`
			AuditStatus *int       `json:"audit_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(real, &resp); err != nil {
		t.Fatalf("真实 OPPO 响应解析失败: %v", err)
	}
	if resp.Data == nil {
		t.Fatal("data 为空")
	}
	if resp.Data.VersionCode == nil || int64(*resp.Data.VersionCode) != 10 {
		t.Errorf("version_code 应为 10，实际 %v", resp.Data.VersionCode)
	}
	if resp.Data.VersionName != "2023.10.10" {
		t.Errorf("version_name 错误: %q", resp.Data.VersionName)
	}
}

// TestRealVivoResponseShape 同理，vivo 的 versionCode 也是字符串。
func TestRealVivoResponseShape(t *testing.T) {
	real := []byte(`{"code":0,"subCode":"0","data":{"cnName":"军队文职真题-真题章节解析",
		"packageName":"tech.lightsoft.civilian","versionName":"2023.10.10",
		"versionCode":"10","status":3,"appType":1,"saleStatus":1,"apkSize":58552}}`)

	var resp struct {
		Code *int `json:"code"`
		Data *struct {
			VersionCode *FlexInt64 `json:"versionCode"`
			VersionName string     `json:"versionName"`
			ReviewState *int       `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(real, &resp); err != nil {
		t.Fatalf("真实 vivo 响应解析失败: %v", err)
	}
	if resp.Data.VersionCode == nil || int64(*resp.Data.VersionCode) != 10 {
		t.Errorf("versionCode 应为 10，实际 %v", resp.Data.VersionCode)
	}
}

// TestRealHonorResponseShape 荣耀的 appId 是裸数字。
func TestRealHonorResponseShape(t *testing.T) {
	real := []byte(`{"code":0,"msg":"Success","data":[{"packageName":"tech.lightsoft.civilian","appId":900876322}]}`)

	var resp struct {
		Code *int `json:"code"`
		Data []struct {
			PackageName string     `json:"packageName"`
			AppID       FlexString `json:"appId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(real, &resp); err != nil {
		t.Fatalf("真实荣耀响应解析失败: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data 长度应为 1，实际 %d", len(resp.Data))
	}
	if got := string(resp.Data[0].AppID); got != "900876322" {
		t.Errorf("appId 应为 \"900876322\"，实际 %q", got)
	}
}
