package publish

import (
	"context"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
)

// 测试用的制品元信息。reader 注入后不读真实文件，因此这里只需与配置包名一致。
func apkInfo(code int64) artifact.Info {
	return artifact.Info{
		Path:          "/tmp/app.apk",
		ApplicationID: testAppID,
		VersionCode:   code,
		VersionName:   "1.0.0",
		Kind:          artifact.KindAPK,
	}
}

func checklistService(
	t *testing.T, cfg config.AppConfig, ch channel.Channel, info artifact.Info,
) *Service {
	t.Helper()
	svc, _ := newTestService(t, cfg, []channel.Channel{ch},
		func(string) (artifact.Info, error) { return info, nil })
	return svc
}

func marketFor(id string, state channel.ReviewState, code int64) channel.MarketInfo {
	info := channel.NewMarketInfo(id, state,
		&channel.Version{Code: code, Name: "1.0.0"}, "")
	return info
}

func findCheck(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("没有找到检查项 %q，实际有：%v", id, checkIDs(checks))
	return Check{}
}

func checkIDs(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.ID)
	}
	return out
}

// 包名不一致必须失败。传错 APK 是最容易发生、后果又最严重的一类失误 ——
// 它会把 A 应用的新版本推到 B 应用的商店页上。
func TestChecklistFailsOnPackageMismatch(t *testing.T) {
	ch := &fakeChannel{id: "huawei"}
	cfg := testConfig("huawei")
	info := apkInfo(10)
	info.ApplicationID = "com.other.app"

	svc := checklistService(t, cfg, ch, info)
	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID,
		ArtifactPath:  "/tmp/app.apk",
		Timeouts:      httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "package-match")
	if got.Status != CheckFail {
		t.Errorf("包名不一致应判失败，实际 %v（%s）", got.Status, got.Detail)
	}
	if len(result.Blocking()) == 0 {
		t.Error("包名不一致必须计入阻塞项")
	}
}

// 版本号低于线上必须失败：那几乎总是选错了制品文件。
func TestChecklistFailsOnLowerVersion(t *testing.T) {
	ch := &fakeChannel{
		id:     "huawei",
		market: ptrMarket(marketFor("huawei", channel.ReviewOnline, 20)),
	}
	svc := checklistService(t, testConfig("huawei"), ch, apkInfo(10))

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: "/tmp/app.apk",
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "version-huawei")
	if got.Status != CheckFail {
		t.Errorf("版本号低于线上应判失败，实际 %v（%s）", got.Status, got.Detail)
	}
}

// 同号提交不算失败，但必须提醒 —— 渠道会拒绝，除非显式开 --allow-same-version。
func TestChecklistWarnsOnSameVersion(t *testing.T) {
	ch := &fakeChannel{
		id:     "huawei",
		market: ptrMarket(marketFor("huawei", channel.ReviewOnline, 10)),
	}
	svc := checklistService(t, testConfig("huawei"), ch, apkInfo(10))

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: "/tmp/app.apk",
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "version-huawei")
	if got.Status != CheckWarn {
		t.Errorf("同号应判提醒，实际 %v（%s）", got.Status, got.Detail)
	}
}

// 渠道正在审核中必须失败：提交会被拒绝。
func TestChecklistFailsWhenUnderReview(t *testing.T) {
	ch := &fakeChannel{
		id:     "huawei",
		market: ptrMarket(marketFor("huawei", channel.ReviewUnderReview, 5)),
	}
	svc := checklistService(t, testConfig("huawei"), ch, apkInfo(10))

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: "/tmp/app.apk",
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "channel-state-huawei")
	if got.Status != CheckFail {
		t.Errorf("审核中应判失败，实际 %v（%s）", got.Status, got.Detail)
	}
}

// 未识别的状态值必须提醒，**不能判通过**。
//
// CanSubmit 是按「不在审核中」推导的缺省值，不是渠道的确认。渠道返回陌生状态码
// 时，真实情况可能是被拒也可能是在审核 —— 拿缺省值当结论正是这个命令要避免的错。
func TestChecklistWarnsOnUnknownState(t *testing.T) {
	ch := &fakeChannel{
		id:     "vivo",
		market: ptrMarket(marketFor("vivo", channel.ReviewUnknown, 11)),
	}
	svc := checklistService(t, testConfig("vivo"), ch, apkInfo(12))

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: "/tmp/app.apk",
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "channel-state-vivo")
	if got.Status != CheckWarn {
		t.Errorf("未知状态应判提醒，实际 %v（%s）", got.Status, got.Detail)
	}
}

// 查询失败算「跳过」，而跳过同样计入阻塞 —— 查不了不等于没问题。
func TestChecklistTreatsUnqueryableChannelAsBlocking(t *testing.T) {
	ch := &fakeChannel{id: "huawei"} // market 为 nil 时 QueryMarket 返回协议错误
	svc := checklistService(t, testConfig("huawei"), ch, apkInfo(10))

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: "/tmp/app.apk",
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findCheck(t, result.Checks, "channel-state-huawei")
	if got.Status != CheckSkip {
		t.Errorf("查询失败应判跳过，实际 %v（%s）", got.Status, got.Detail)
	}
	if len(result.Blocking()) == 0 {
		t.Error("跳过项必须计入阻塞，否则「查不了」会被当成「没问题」")
	}
}

// 制品读不出来时要返回一份可读的报告，而不是光秃秃的解析错误 ——
// 后面那些检查没做完，报告里要说明。
func TestChecklistReportsUnreadableArtifact(t *testing.T) {
	ch := &fakeChannel{
		id:     "huawei",
		market: ptrMarket(marketFor("huawei", channel.ReviewOnline, 5)),
	}
	svc, apkPath := newTestService(t, testConfig("huawei"), []channel.Channel{ch},
		func(string) (artifact.Info, error) {
			return artifact.Info{}, errTestRead
		})

	result, err := svc.Checklist(context.Background(), ChecklistOptions{
		ApplicationID: testAppID, ArtifactPath: apkPath,
		Timeouts: httpx.Default(),
	})
	if err != nil {
		t.Fatalf("制品读不出来时不该直接返回错误，而应给出报告：%v", err)
	}
	if len(result.Checks) != 1 {
		t.Fatalf("应只有一条关于制品的失败项，实际 %v", checkIDs(result.Checks))
	}
	if result.Checks[0].Status != CheckFail {
		t.Errorf("应判失败，实际 %v", result.Checks[0].Status)
	}
}

var errTestRead = &testError{"无法解析制品"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func ptrMarket(m channel.MarketInfo) *channel.MarketInfo { return &m }
