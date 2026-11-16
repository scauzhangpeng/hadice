// Package mcpserver 将 Hadice 的设备调试能力（adb / hdc）封装为 MCP 工具，
// 供 WorkBuddy、Codex 等 MCP 客户端接入，实现"连接手机 + 各项调试能力"的自动化。
//
// 架构（工具只在 App 进程内执行，不存在独立运行的副本）：
//
//	┌─────────────────────────────────────────────────────────────┐
//	│ Hadice App（打包产物，运行中）                                │
//	│   backend.Run() → 内嵌 MCP streamable-http（127.0.0.1 随机端口）│
//	│   · 工具在这里注册与执行，Runtime 复用 GUI 的 deviceManager      │
//	│   · 端点写入 discovery 文件（端点 + PID）                       │
//	└─────────────────────────────────────────────────────────────┘
//	                       ▲ 桥接（bridge.go，工具清单与调用原样透传）
//	┌─────────────────────────────────────────────────────────────┐
//	│ hadice-mcp（stdio 子进程，WorkBuddy / Codex 各自拉起一份）      │
//	│   不含任何设备能力，只做协议转换；App 未运行则明确报错退出        │
//	└─────────────────────────────────────────────────────────────┘
//
// 设计要点：
//
//   - 能力只有一份：工具定义与实现都在 App 内，桥接端不重复实现，避免两套
//     设备状态与行为差异；新增工具无需改动 hadice-mcp。
//
//   - 依赖注入：工具通过 *Runtime 访问设备能力，Runtime 由 App 注入全局
//     deviceManager，不使用包级全局状态。
//
//   - 工具按域拆分文件（devices.go / apps.go / packages.go / input.go），
//     新增工具只需实现一个 handler 并在对应 register*Tools 里追加注册。
package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// ServerName 服务标识，即 MCP 客户端（WorkBuddy 等）里展示的服务名。
	ServerName = "hadice-mcp"
	// ServerTitle 人类可读的服务标题。
	ServerTitle = "Hadice 移动调试 MCP 服务"
	// ServerVersion MCP 服务自身的协议层版本，与 build/config.yml 的应用版本独立演进。
	ServerVersion = "v0.1.0"
)

// New 构建一个已完成工具注册的 MCP Server。
// 与传输方式无关：调用方决定用 StdioTransport 还是 ServeHTTP 运行它。
// rt 由 App 注入（共享 GUI 的 deviceManager）。
func New(rt *Runtime) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Title:   ServerTitle,
		Version: ServerVersion,
	}, nil)

	rt.registerDeviceTools(s)
	rt.registerAppTools(s)
	rt.registerPackageTools(s)
	rt.registerInputTools(s)
	rt.registerResources(s)

	return s
}
