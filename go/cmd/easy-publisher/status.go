package main

import (
	"errors"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/httpx"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var (
		app        string
		channels   []string
		timeoutSec int64
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "查询应用在各渠道的审核状态与线上版本号",
		Long: "查询应用在各渠道的审核状态与线上版本号。\n\n" +
			"发版前应当先跑这个命令：如果某渠道正在审核中，提交新版本会被拒绝。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(app)
			if err != nil {
				return err
			}
			svc := publish.NewService(config.NewStore(""))
			defer svc.Close()

			timeouts := httpx.Default()
			if timeoutSec > 0 {
				timeouts = httpx.OfSeconds(timeoutSec)
			}

			results, err := svc.MarketStates(cmd.Context(), applicationID, channels, timeouts)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				return eperr.ConfigurationError(
					"应用 %s 没有启用任何渠道，用 `easy-publisher channel toggle --app %s "+
						"--channel <渠道> --enable` 启用", applicationID, applicationID)
			}

			// 保持渠道注册顺序，输出才稳定
			order := make([]string, 0, len(results))
			for _, ch := range channel.All() {
				if _, ok := results[ch.ID()]; ok {
					order = append(order, ch.ID())
				}
			}

			if jsonOut {
				type reviewNoteJSON struct {
					Kind    string  `json:"kind"`
					Passed  *bool   `json:"passed"`
					Opinion *string `json:"opinion"`
				}
				type channelJSON struct {
					ID               string  `json:"id"`
					OK               bool    `json:"ok"`
					ReviewState      *string `json:"reviewState"`
					ReviewStateLabel *string `json:"reviewStateLabel"`
					CanSubmit        *bool   `json:"canSubmit"`
					LastVersionCode  *int64  `json:"lastVersionCode"`
					LastVersionName  *string `json:"lastVersionName"`
					RawState         *string `json:"rawState"`
					// RejectReason 是渠道给出的审核意见原文
					RejectReason *string `json:"rejectReason"`
					// RejectAttachments 是审核意见附件的 URL（审核员截图等）
					RejectAttachments []string `json:"rejectAttachments,omitempty"`
					// ReviewNotes 是渠道按维度给出的补充意见（华为的版权/版号/备案）
					ReviewNotes []reviewNoteJSON `json:"reviewNotes,omitempty"`
					Kind        *string          `json:"kind"`
					Code        *string          `json:"code"`
					Message     *string          `json:"message"`
					Retryable   *bool            `json:"retryable"`
				}
				out := struct {
					OK            bool          `json:"ok"`
					ApplicationID string        `json:"applicationId"`
					Channels      []channelJSON `json:"channels"`
				}{OK: true, ApplicationID: applicationID}

				for _, id := range order {
					item := channelJSON{ID: id}
					res := results[id]
					if res.Err != nil {
						var pe *eperr.Error
						item.OK = false
						msg := res.Err.Error()
						item.Message = &msg
						if asEperr(res.Err, &pe) {
							k, c := pe.Kind.String(), pe.Code
							r := pe.Retryable()
							item.Kind, item.Retryable = &k, &r
							if c != "" {
								item.Code = &c
							}
						}
					} else {
						item.OK = true
						state, label := res.Info.ReviewState.String(), res.Info.ReviewState.Label()
						can := res.Info.CanSubmit
						item.ReviewState, item.ReviewStateLabel, item.CanSubmit = &state, &label, &can
						if v := res.Info.LastVersion; v != nil {
							item.LastVersionCode, item.LastVersionName = &v.Code, &v.Name
						}
						if res.Info.RawState != "" {
							raw := res.Info.RawState
							item.RawState = &raw
						}
						if r := res.Info.Review; r != nil {
							if r.Opinion != "" {
								opinion := r.Opinion
								item.RejectReason = &opinion
							}
							item.RejectAttachments = r.Attachments
							for _, n := range r.Notes {
								note := reviewNoteJSON{Kind: n.Kind, Passed: n.Passed}
								if n.Opinion != "" {
									o := n.Opinion
									note.Opinion = &o
								}
								item.ReviewNotes = append(item.ReviewNotes, note)
							}
						}
					}
					out.Channels = append(out.Channels, item)
				}
				if err := output.JSON(out); err != nil {
					return err
				}
			} else {
				rows := make([][]string, 0, len(order))
				for _, id := range order {
					res := results[id]
					if res.Err != nil {
						rows = append(rows, []string{id, "查询失败", "-", "-"})
						continue
					}
					version := "无"
					if v := res.Info.LastVersion; v != nil {
						version = v.String()
					}
					canSubmit := "否"
					if res.Info.CanSubmit {
						canSubmit = "是"
					}
					rows = append(rows, []string{id, res.Info.ReviewState.Label(), version, canSubmit})
				}
				output.Table([]string{"渠道", "审核状态", "线上版本", "可提交"}, rows)

				// 审核意见单独列出：它是一段自由文本（华为限 1024 字符），
				// 塞进表格会把列宽撑爆。这是排查拒审时最有用的信息，值得占几行
				printReviewFeedback(order, results)

				for _, id := range order {
					if err := results[id].Err; err != nil {
						output.Line("")
						output.ReportError(err, false)
					}
				}
			}

			// 有渠道查询失败时用非零退出码，CI 才能感知
			for _, id := range order {
				if err := results[id].Err; err != nil {
					var pe *eperr.Error
					if asEperr(err, &pe) {
						return &silentError{code: output.ExitCodeOf(pe.Kind)}
					}
					return &silentError{code: output.ExitUsage}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "包名（必填）")
	cmd.Flags().StringSliceVar(&channels, "channel", nil, "只查指定渠道，逗号分隔")
	cmd.Flags().Int64Var(&timeoutSec, "timeout", 0, "单次请求超时秒数，默认 120")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}

// silentError 表示「错误已经输出过了，只需要按这个退出码退出」。
//
// status 会逐渠道打印失败原因，若再让 main 里统一报一次就会重复。
type silentError struct{ code int }

func (e *silentError) Error() string { return "" }

// ExitCode 让 main 能取出期望的退出码。
func (e *silentError) ExitCode() int { return e.code }

func asEperr(err error, target **eperr.Error) bool { return errors.As(err, target) }

// printReviewFeedback 打印各渠道给出的审核意见。
//
// 只有部分渠道提供，且都是一段自由文本 —— 没有一家给出结构化的原因码，
// 因此这里只做转述，不做分类或归纳：原文交给人判断，比我们猜得准。
func printReviewFeedback(order []string, results map[string]publish.MarketResult) {
	for _, id := range order {
		res := results[id]
		if res.Err != nil || res.Info.Review == nil {
			continue
		}
		feedback := res.Info.Review
		output.Line("")
		output.Line("[%s] %s", id, res.Info.ReviewState.Label())
		if feedback.Opinion != "" {
			output.Line("  审核意见：%s", feedback.Opinion)
		}
		// 华为会按维度分别给出结果（版权/版号/备案），其它渠道通常没有
		for _, note := range feedback.Notes {
			verdict := ""
			if note.Passed != nil {
				if *note.Passed {
					verdict = "通过"
				} else {
					verdict = "不通过"
				}
			}
			switch {
			case verdict != "" && note.Opinion != "":
				output.Line("  %s：%s（%s）", note.Kind, verdict, note.Opinion)
			case verdict != "":
				output.Line("  %s：%s", note.Kind, verdict)
			case note.Opinion != "":
				output.Line("  %s：%s", note.Kind, note.Opinion)
			}
		}
		// 附件往往是审核员截的图，有时比文字说明更能说明问题
		for _, url := range feedback.Attachments {
			output.Line("  附件：%s", url)
		}
	}
}
