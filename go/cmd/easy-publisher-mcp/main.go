// Command easy-publisher-mcp 是 easy-publisher 的 MCP server（stdio transport）。
//
// # stdout 必须被保护
//
// stdio transport 用 stdout 独占传输 JSON-RPC 报文，任何一行多余输出都会让宿主
// 解析失败，而且症状是「server 莫名连不上」，极难定位到某个 Println。
//
// 这里不靠约定：logx 在构造时就把 handler 绑到 os.Stderr，包内没有任何写 stdout
// 的路径。业务代码里也不使用 fmt.Println —— 需要输出时走 logx 或工具的返回值。
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/fangxiangCN/easy-publisher/go/internal/builtin"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/logx"
	"github.com/fangxiangCN/easy-publisher/go/internal/publish"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "1.0.0"

func main() {
	builtin.Register()

	// 日志一律 stderr，且写文件。stdio 场景下 stdout 是协议通道
	logx.Configure(logx.LevelInfo, config.HomeDir()+"/log")
	logx.Info("easy-publisher MCP server 启动", "version", version)

	svc := publish.NewService(config.NewStore(""))
	defer svc.Close()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "easy-publisher",
		Version: version,
	}, nil)

	registerTools(server, svc)

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, context.Canceled) {
		logx.Error("MCP server 退出", "err", err)
		logx.Flush()
		os.Exit(1)
	}
	logx.Info("MCP server 正常退出")
	logx.Flush()
}
