package main

import (
	"os"
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/spf13/cobra"
)

func newUploadCmd() *cobra.Command {
	var (
		app              string
		artifactPath     string
		desc             string
		channels         []string
		onlineTime       string
		stopAfter        string
		allowSameVersion bool
		skipVersionCheck bool
		timeoutSec       int64
		jsonOut          bool
	)
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "上传制品并提交新版本",
		Long: "上传 APK（或鸿蒙的 .app）并提交新版本。\n\n" +
			"注意：提交后无法通过 API 撤回 —— 各应用商店都不提供撤销版本更新的接口。\n" +
			"建议先用 `status` 确认各渠道状态。\n\n" +
			"默认等待全部渠道完成并实时显示进度。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(app)
			if err != nil {
				return err
			}

			scheduledAt := int64(0)
			if onlineTime != "" {
				scheduledAt, err = publish.ParseOnlineTime(onlineTime)
				if err != nil {
					return err
				}
			}

			rule := publish.VersionStrict
			switch {
			case skipVersionCheck:
				rule = publish.VersionSkip
			case allowSameVersion:
				rule = publish.VersionAllowSame
			}

			var stopPtr *channel.ReleaseStage
			if strings.TrimSpace(stopAfter) != "" {
				stage, specified, err := channel.ParseStage(stopAfter)
				if err != nil {
					return err
				}
				if specified {
					stopPtr = &stage
				}
			}

			timeouts := httpx.Default()
			if timeoutSec > 0 {
				timeouts = httpx.OfSeconds(timeoutSec)
			}

			svc := publish.NewService(config.NewStore(""))
			defer svc.Close()

			jobID, err := svc.Submit(cmd.Context(), publish.SubmitOptions{
				ApplicationID: applicationID,
				ArtifactPath:  artifactPath,
				Release: channel.ReleaseParams{
					UpdateDesc: desc,
					OnlineTime: scheduledAt,
				},
				ChannelIDs: channels,
				Version:    rule,
				Timeouts:   timeouts,
				StopAfter:  stopPtr,
			})
			if err != nil {
				return err
			}

			job := waitForJob(cmd, svc, jobID, jsonOut)
			return reportJob(job, jsonOut)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "包名（必填）")
	cmd.Flags().StringVar(&artifactPath, "artifact", "",
		"制品文件（.apk，鸿蒙为 .app），或存放多渠道包的目录（必填）")
	// 别名：早期文档与脚本用的是 --apk
	cmd.Flags().StringVar(&artifactPath, "apk", "", "同 --artifact（别名）")
	_ = cmd.Flags().MarkHidden("apk")
	cmd.Flags().StringVar(&artifactPath, "file", "", "同 --artifact（别名）")
	_ = cmd.Flags().MarkHidden("file")
	cmd.Flags().StringVar(&desc, "desc", "", "更新说明（必填）")
	cmd.Flags().StringSliceVar(&channels, "channel", nil, "只发指定渠道，逗号分隔")
	cmd.Flags().StringVar(&onlineTime, "online-time", "",
		"定时上线时间，格式 yyyy-MM-dd HH:mm:ss，不填则审核通过后立即发布")
	cmd.Flags().StringVar(&stopAfter, "stop-after", "",
		"流程走到哪一步就停下：artifact=仅上传安装包（不创建版本）、"+
			"draft=停在草稿态（可先到渠道后台核对再送审）、submit=一路走到送审。"+
			"不指定则各渠道走到各自能到的最远阶段 —— 多数渠道是送审。\n"+
			"并非所有渠道都支持中途停下：小米的 dev/push 是原子的只能 submit，"+
			"OPPO/vivo 没有草稿态")
	cmd.Flags().BoolVar(&allowSameVersion, "allow-same-version", false,
		"允许版本号与线上相同（审核被拒后仅更新素材时使用）。版本号低于线上仍会被拒绝")
	cmd.Flags().BoolVar(&skipVersionCheck, "skip-version-check", false,
		"跳过全部版本号校验，仅用于排查问题")
	cmd.Flags().Int64Var(&timeoutSec, "timeout", 0, "单次请求超时秒数，默认 120")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	for _, flag := range []string{"app", "artifact", "desc"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

// waitForJob 轮询直到任务结束。
//
// 进度在非 JSON 模式下刷新到 stderr；JSON 模式静默等待，只输出最终结果，
// 避免中间态污染 stdout 的结构化输出。
func waitForJob(cmd *cobra.Command, svc *publish.Service, jobID string, jsonOut bool) publish.Job {
	lastLine := ""
	for {
		job, ok := svc.Job(jobID)
		if !ok {
			return publish.Job{}
		}
		if !jsonOut {
			parts := make([]string, 0, len(job.Channels))
			for _, c := range job.Channels {
				parts = append(parts, c.ChannelID+":"+c.Stage.Label())
			}
			line := strings.Join(parts, "  ")
			if line != lastLine {
				// 进度走 stderr，stdout 只留最终结果
				os.Stderr.WriteString("\r" + line)
				lastLine = line
			}
		}
		if job.Done() {
			if !jsonOut && lastLine != "" {
				os.Stderr.WriteString("\n")
			}
			return job
		}
		select {
		case <-cmd.Context().Done():
			svc.Cancel(jobID)
			j, _ := svc.Job(jobID)
			return j
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// reportJob 输出任务结果并按状态返回退出码。
func reportJob(job publish.Job, jsonOut bool) error {
	if jsonOut {
		type channelJSON struct {
			ID           string  `json:"id"`
			State        string  `json:"state"`
			Message      string  `json:"message"`
			Reached      *string `json:"reachedStage"`
			ReachedLabel *string `json:"reachedStageLabel"`
			Kind         *string `json:"kind"`
			KindLabel    *string `json:"kindLabel"`
			Code         *string `json:"code"`
			Retryable    *bool   `json:"retryable"`
			// Phase 解释「为什么不可重试」
			Phase  *string `json:"phase"`
			Detail *string `json:"detail"`
		}
		out := struct {
			OK             bool          `json:"ok"`
			JobID          string        `json:"jobId"`
			State          string        `json:"state"`
			ApplicationID  string        `json:"applicationId"`
			VersionCode    int64         `json:"versionCode"`
			VersionName    string        `json:"versionName"`
			RequestedStage *string       `json:"requestedStage"`
			Channels       []channelJSON `json:"channels"`
		}{
			OK: job.State() == publish.JobSucceeded, JobID: job.ID,
			State: job.State().String(), ApplicationID: job.ApplicationID,
			VersionCode: job.VersionCode, VersionName: job.VersionName,
		}
		if job.RequestedStage != nil {
			s := job.RequestedStage.String()
			out.RequestedStage = &s
		}
		for _, c := range job.Channels {
			item := channelJSON{
				ID: c.ChannelID, State: c.Stage.Kind.String(),
				Message: c.Stage.Label(),
			}
			if c.Stage.Kind == publish.StageSucceeded {
				r, l := c.Stage.Reached.String(), c.Stage.Reached.Label()
				item.Reached, item.ReachedLabel = &r, &l
			}
			if c.Stage.Kind == publish.StageFailed {
				k, kl, ph, d := c.Stage.ErrKind.String(), c.Stage.ErrKind.Label(),
					c.Stage.Phase.String(), c.Stage.Message
				retry := c.Stage.Retryable
				item.Kind, item.KindLabel, item.Phase, item.Detail, item.Retryable = &k, &kl, &ph, &d, &retry
				if c.Stage.Code != "" {
					code := c.Stage.Code
					item.Code = &code
				}
			}
			out.Channels = append(out.Channels, item)
		}
		if err := output.JSON(out); err != nil {
			return err
		}
	} else {
		output.Line("")
		output.Line("%s  %s(%d)", job.ApplicationID, job.VersionName, job.VersionCode)
		rows := make([][]string, 0, len(job.Channels))
		for _, c := range job.Channels {
			rows = append(rows, []string{c.ChannelID, c.Stage.Label()})
		}
		output.Table([]string{"渠道", "结果"}, rows)

		if failures := job.Failed(); len(failures) > 0 {
			output.Line("")
			for _, f := range failures {
				os.Stderr.WriteString("[" + f.ChannelID + "] " + f.Stage.Message + "\n")
				if f.Stage.Retryable {
					os.Stderr.WriteString("  该错误通常可重试\n")
				}
			}
		}
	}

	// 部分渠道成功时退出码也必须非零，否则 CI 会当成全部成功
	switch job.State() {
	case publish.JobSucceeded:
		return nil
	case publish.JobCancelled:
		return &silentError{code: output.ExitUsage}
	default:
		if failures := job.Failed(); len(failures) > 0 {
			return &silentError{code: output.ExitCodeOf(failures[0].Stage.ErrKind)}
		}
		return &silentError{code: output.ExitChannelRejected}
	}
}
