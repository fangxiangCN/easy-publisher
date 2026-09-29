package main

import (
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/spf13/cobra"
)

func newChecklistCmd() *cobra.Command {
	var (
		app         string
		artifactArg string
		channels    []string
		expectLabel string
		timeoutSec  int64
		jsonOut     bool
	)

	cmd := &cobra.Command{
		Use:   "checklist",
		Short: "上架前逐项检查（只读）",
		Long: "上架前逐项检查。**这是只读命令，不会提交任何东西。**\n\n" +
			"检查两类内容：\n" +
			"  1. 制品本身：包名与配置是否一致、应用名是否还是脚手架占位符、\n" +
			"     图标是否还是模板图、版本号是否高于各渠道线上版本；\n" +
			"  2. 渠道状态：是否正在审核中（审核中的渠道会被拒绝提交）。\n\n" +
			"每一项都对应一类真实发生过的驳回（应用名不一致、模板图标、\n" +
			"版本号未递增、渠道审核中）。\n\n" +
			"结论为「跳过」的项同样算阻塞 —— 查不了不等于没问题。\n" +
			"退出码：全部通过（含提醒）为 0，有阻塞项为 5。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(app)
			if err != nil {
				return err
			}

			timeouts := httpx.Default()
			if timeoutSec > 0 {
				timeouts = httpx.OfSeconds(timeoutSec)
			}

			svc := publish.NewService(config.NewStore(""))
			defer svc.Close()

			result, err := svc.Checklist(cmd.Context(), publish.ChecklistOptions{
				ApplicationID: applicationID,
				ArtifactPath:  artifactArg,
				ChannelIDs:    channels,
				ExpectLabel:   expectLabel,
				Timeouts:      timeouts,
			})
			if err != nil {
				return err
			}

			if jsonOut {
				printChecklistJSON(result)
			} else {
				printChecklistText(result)
			}

			if blocking := result.Blocking(); len(blocking) > 0 {
				return &silentError{code: output.ExitPrecondition}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&app, "app", "", "包名，如 com.example.app（必填）")
	cmd.Flags().StringVar(&artifactArg, "artifact", "",
		"待上架的制品路径（.apk / 鸿蒙 .app）")
	cmd.Flags().StringSliceVar(&channels, "channel", nil,
		"只检查指定渠道；省略则检查全部已启用渠道")
	cmd.Flags().StringVar(&expectLabel, "expect-label", "",
		"商店页展示的应用名，用于比对 APK 内的 android:label")
	cmd.Flags().Int64Var(&timeoutSec, "timeout", 0, "单次请求超时秒数，默认 120")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	_ = cmd.MarkFlagRequired("app")

	return cmd
}

// statusMarker 把检查结论映射成一眼能分辨的标记。
//
// 用文字而不是 emoji：日志会被重定向到文件、贴进 issue，emoji 在那些场景
// 里宽度不固定，反而更难对齐。
func statusMarker(s publish.CheckStatus) string {
	switch s {
	case publish.CheckPass:
		return "OK  "
	case publish.CheckWarn:
		return "提醒"
	case publish.CheckFail:
		return "失败"
	default:
		return "跳过"
	}
}

func printChecklistText(result publish.ChecklistResult) {
	output.Line("应用：%s", result.ApplicationID)
	if result.Artifact.Path != "" {
		output.Line("制品：%s", result.Artifact.String())
	}
	output.Line("")

	for _, c := range result.Checks {
		output.Line("[%s] %s", statusMarker(c.Status), c.Title)
		if c.Detail != "" {
			for _, line := range strings.Split(c.Detail, "\n") {
				output.Line("         %s", line)
			}
		}
		if c.Fix != "" && c.Status != publish.CheckPass {
			output.Line("         建议：%s", c.Fix)
		}
	}

	failed, blocking := 0, result.Blocking()
	for _, c := range result.Checks {
		if c.Status == publish.CheckFail {
			failed++
		}
	}

	output.Line("")
	switch {
	case failed > 0:
		output.Line("结论：%d 项不通过、共 %d 项阻塞，先修复再上架。",
			failed, len(blocking))
	case len(blocking) > 0:
		output.Line("结论：%d 项未能检查（跳过），无法确认这些项没问题，先查清再说。",
			len(blocking))
	default:
		output.Line("结论：全部通过。可以上架。")
	}
}

func printChecklistJSON(result publish.ChecklistResult) {
	type checkJSON struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
		Fix    string `json:"fix,omitempty"`
	}
	type artifactJSON struct {
		Path          string `json:"path"`
		ApplicationID string `json:"applicationId"`
		VersionCode   int64  `json:"versionCode"`
		VersionName   string `json:"versionName"`
		SizeBytes     int64  `json:"sizeBytes"`
	}
	out := struct {
		OK            bool          `json:"ok"`
		ApplicationID string        `json:"applicationId"`
		Artifact      *artifactJSON `json:"artifact,omitempty"`
		Checks        []checkJSON   `json:"checks"`
		FailedCount   int           `json:"failedCount"`
		BlockedCount  int           `json:"blockedCount"`
		Ready         bool          `json:"ready"`
	}{OK: true, ApplicationID: result.ApplicationID, Checks: []checkJSON{}}

	if result.Artifact.Path != "" {
		a := result.Artifact
		out.Artifact = &artifactJSON{
			Path:          a.Path,
			ApplicationID: a.ApplicationID,
			VersionCode:   a.VersionCode,
			VersionName:   a.VersionName,
			SizeBytes:     a.SizeBytes,
		}
	}
	for _, c := range result.Checks {
		out.Checks = append(out.Checks, checkJSON{
			ID:     c.ID,
			Title:  c.Title,
			Status: c.Status.String(),
			Detail: c.Detail,
			Fix:    c.Fix,
		})
	}
	out.BlockedCount = len(result.Blocking())
	out.FailedCount = len(result.Failed())
	out.Ready = out.BlockedCount == 0
	_ = output.JSON(out)
}
