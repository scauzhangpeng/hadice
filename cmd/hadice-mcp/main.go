// hadice-mcp 是 Hadice App 的 MCP 接入进程，供 WorkBuddy、Codex 等客户端以
// stdio 子进程方式配置（各客户端一份进程，配置完全相同）。
//
// 它本身不操作设备（不执行任何 adb/hdc 命令）：启动后先发现运行中的 Hadice App
// （discovery 文件 + PID + healthz），再把 App 内嵌 MCP 端点暴露的工具原样
// 透传到 stdio。能力全部来自 App，因此使用前 App 必须处于运行状态。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"Hadice/backend/mcpserver"
)

func main() {
	// stdio 模式下 stdout 是 JSON-RPC 协议通道，所有日志必须走 stderr。
	log.SetOutput(os.Stderr)

	wait := flag.Duration("wait", 10*time.Second,
		"等待 Hadice App 就绪的最长时间（客户端可能先于 App 启动）；0 表示只探测一次")
	flag.Parse()

	if err := mcpserver.BridgeStdio(context.Background(), *wait); err != nil {
		log.Printf("[%s] %v", mcpserver.ServerName, err)
		os.Exit(1)
	}
}
