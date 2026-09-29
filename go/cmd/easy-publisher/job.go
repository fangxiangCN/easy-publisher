package main

import (
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/spf13/cobra"
)

func newJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "查询上传任务",
		Long: "查询上传任务。\n\n" +
			"注意：任务状态保存在进程内存里，CLI 进程退出后即失效。\n" +
			"`upload` 默认等待完成，因此这个命令主要用于排查。",
	}
	cmd.AddCommand(newJobStatusCmd(), newJobListCmd())
	return cmd
}

func newJobListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出本进程内的任务",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// CLI 每次执行都是新进程，所以这里必然是空的。
			// 保留这个命令是为了与 MCP 的语义对齐，并让使用者明白状态不跨进程
			out := map[string]any{"ok": true, "jobIds": []string{}}
			if jsonOut {
				return output.JSON(out)
			}
			output.Line("当前进程没有任务（任务状态不跨进程保留）")
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	return cmd
}

func newJobStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status <jobId>",
		Short: "查询指定任务的进度",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// CLI 是单次执行的进程，任务状态在 upload 的那个进程里，
			// 这里查不到 —— 给出明确的解释而不是含糊的「任务不存在」
			return eperr.ConfigurationError(
				"任务状态不跨进程保留，CLI 每次执行都是新进程，因此查不到 %s。\n"+
					"`upload` 默认会等待任务完成并直接输出结果；"+
					"任务查询主要用于 MCP server 那种长驻进程", args[0])
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	return cmd
}
