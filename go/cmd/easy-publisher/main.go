// Command easy-publisher 是一个把 APK 提交到多个应用商店的命令行工具。
//
// # 两条约定
//
//  1. **stdout 只放结果，日志一律走 stderr**（由 logx 在构造时绑死）。
//     这样 `--json` 的输出可以直接管道给 jq。
//  2. **退出码按错误类型区分**，见 output.ExitCodes。
package main

import (
	"errors"
	"os"

	"github.com/fangxiangCN/easy-publisher/go/internal/builtin"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/spf13/cobra"
)

func main() {
	// 注册内置渠道。CLI 与 MCP 都从这里拿到同一批渠道
	builtin.Register()

	root := newRootCmd()
	if err := root.Execute(); err != nil {
		// silentError 表示错误已经输出过了（status 会逐渠道打印失败原因），
		// 只需要按期望的退出码退出，不要重复报错
		var silent *silentError
		if errors.As(err, &silent) {
			os.Exit(silent.ExitCode())
		}
		os.Exit(output.ReportError(err, jsonFlagOf(root)))
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "easy-publisher",
		Short: "一键把 APK 提交到多个应用商店",
		Long: "一键把 APK 提交到多个应用商店。\n\n" +
			"支持华为、小米、OPPO、vivo、荣耀、鸿蒙。凭据保存在 ~/.easy-publisher/ 下\n" +
			"（权限 600），也可用环境变量 EP_<渠道>_<参数> 覆盖，CI 场景无需落盘。\n\n" +
			"日志写 stderr，结果写 stdout，可直接管道给 jq 处理。",
		// 子命令自己处理错误输出（要区分 --json），这里不再重复打印
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolP("verbose", "v", false, "输出调试日志到 stderr")
	root.PersistentFlags().BoolP("quiet", "q", false, "只输出错误日志")
	root.PersistentFlags().Bool("no-file-log", false, "不写日志文件")

	root.AddCommand(
		newAppCmd(),
		newChannelCmd(),
		newStatusCmd(),
		newUploadCmd(),
		newJobCmd(),
	)
	return root
}

// jsonFlagOf 在错误路径上判断要不要输出结构化错误。
//
// 这里读的是 flag 而不是子命令的变量：错误可能在任何子命令里产生，
// 根命令只关心「调用方有没有要求 JSON」。
func jsonFlagOf(root *cobra.Command) bool {
	flag := root.Flags().Lookup("json")
	if flag == nil {
		// --json 定义在子命令上，遍历找到被调用的那个
		cmd, _, err := root.Find(os.Args[1:])
		if err == nil && cmd != nil {
			flag = cmd.Flags().Lookup("json")
		}
	}
	return flag != nil && flag.Value.String() == "true"
}
