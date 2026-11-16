package mcpserver

// 客户端接入模板。
//
// 各家 MCP 客户端的配置文件位置与格式差异很大（JSON / TOML / YAML，
// mcpServers / servers / context_servers / mcp 等不同键名），照抄很容易出错。
// 这里把常见的 16 种做成模板，按当前运行状态填好路径与端点，用户复制即可。
//
// 默认推荐 stdio（command 指向 hadice-mcp），因为：
//   - 客户端自己拉起子进程、自己回收，无需用户关心生命周期；
//   - 桥接进程会随 App 重启自动重连，配置写一次不用改。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TemplateVars 是渲染模板需要的运行时信息。
type TemplateVars struct {
	// Command hadice-mcp 的绝对路径。
	Command string
	// Endpoint TCP 端点（http://127.0.0.1:port/mcp）。
	Endpoint string
	// Token TCP 端点 token。
	Token string
}

// Template 是一份可直接复制的客户端配置。
type Template struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Format  string `json:"format"`
	Path    string `json:"path"`
	Note    string `json:"note"`
	Content string `json:"content"`
}

// RenderTemplates 按当前运行状态渲染全部客户端模板。
func RenderTemplates(v TemplateVars) []Template {
	cmd := v.Command
	if cmd == "" {
		cmd = "/Applications/Hadice.app/Contents/MacOS/hadice-mcp"
	}
	args := []string{"-wait", "30s"}

	// stdio 版通用 JSON（mcpServers 键，Claude/Cursor/Windsurf 等大多数客户端通用）。
	stdio := map[string]any{
		"mcpServers": map[string]any{
			"hadice": map[string]any{
				"command": cmd,
				"args":    args,
			},
		},
	}
	stdioJSON := mustJSON(stdio)

	vscode := mustJSON(map[string]any{
		"servers": map[string]any{
			"hadice": map[string]any{
				"type":    "stdio",
				"command": cmd,
				"args":    args,
			},
		},
	})

	zed := mustJSON(map[string]any{
		"context_servers": map[string]any{
			"hadice": map[string]any{
				"command": map[string]any{"path": cmd, "args": args},
			},
		},
	})

	opencode := mustJSON(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp": map[string]any{
			"hadice": map[string]any{
				"type":    "local",
				"command": append([]string{cmd}, args...),
				"enabled": true,
			},
		},
	})

	codex := strings.Join([]string{
		"[mcp_servers.hadice]",
		fmt.Sprintf("command = %q", cmd),
		`args = ["-wait", "30s"]`,
		"",
	}, "\n")

	cont := strings.Join([]string{
		"mcpServers:",
		"  - name: hadice",
		fmt.Sprintf("    command: %s", cmd),
		"    args:",
		"      - -wait",
		"      - 30s",
		"",
	}, "\n")

	// HTTP 版：只给必须要 URL 的客户端（部分 IDE 插件 / 远程客户端）。
	// token 必须带上，否则会被 401 拒绝。
	httpJSON := mustJSON(map[string]any{
		"mcpServers": map[string]any{
			"hadice": map[string]any{
				"url": v.Endpoint,
				"headers": map[string]string{
					"Authorization": "Bearer " + v.Token,
				},
			},
		},
	})

	cli := fmt.Sprintf("claude mcp add hadice -- %s -wait 30s", cmd)

	return []Template{
		{
			ID: "workbuddy", Name: "WorkBuddy", Format: "json",
			Path:    "设置 → 连接器 → 自定义 MCP",
			Note:    "把 command 指向 hadice-mcp 二进制；args 保留 -wait 30s，App 启动较慢时仍能连上。",
			Content: stdioJSON,
		},
		{
			ID: "claude-code", Name: "Claude Code", Format: "command",
			Path:    "~/.claude.json（或直接执行命令）",
			Note:    "推荐直接执行命令；也可把 JSON 合并进 ~/.claude.json 的 mcpServers。",
			Content: cli + "\n\n" + stdioJSON,
		},
		{
			ID: "claude-desktop", Name: "Claude Desktop", Format: "json",
			Path:    "~/Library/Application Support/Claude/claude_desktop_config.json",
			Note:    "修改后需完全退出 Claude Desktop 再启动。",
			Content: stdioJSON,
		},
		{
			ID: "cursor", Name: "Cursor", Format: "json",
			Path:    "~/.cursor/mcp.json",
			Note:    "保存后在 Settings → MCP 里确认已连接。",
			Content: stdioJSON,
		},
		{
			ID: "vscode", Name: "VS Code", Format: "json",
			Path:    ".vscode/mcp.json（工作区）",
			Note:    "VS Code 用 servers 键，且必须声明 type。",
			Content: vscode,
		},
		{
			ID: "windsurf", Name: "Windsurf", Format: "json",
			Path:    "~/.codeium/windsurf/mcp_config.json",
			Note:    "保存后在 Cascade 的 MCP 面板刷新。",
			Content: stdioJSON,
		},
		{
			ID: "cline", Name: "Cline", Format: "json",
			Path:    "~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json",
			Note:    "也可在 Cline 面板 → MCP Servers → Configure 里粘贴。",
			Content: stdioJSON,
		},
		{
			ID: "continue", Name: "Continue", Format: "yaml",
			Path:    "~/.continue/config.yaml",
			Note:    "YAML 格式，注意缩进。",
			Content: cont,
		},
		{
			ID: "codex", Name: "Codex CLI", Format: "toml",
			Path:    "~/.codex/config.toml",
			Note:    "TOML 格式，追加到文件末尾即可。",
			Content: codex,
		},
		{
			ID: "gemini-cli", Name: "Gemini CLI", Format: "json",
			Path:    "~/.gemini/settings.json",
			Note:    "与 mcpServers 键通用。",
			Content: stdioJSON,
		},
		{
			ID: "opencode", Name: "OpenCode", Format: "json",
			Path:    "opencode.json（项目根或全局配置）",
			Note:    "OpenCode 的 command 是数组，且用 mcp 键。",
			Content: opencode,
		},
		{
			ID: "zed", Name: "Zed", Format: "json",
			Path:    "~/.config/zed/settings.json",
			Note:    "Zed 用 context_servers，command 是对象。",
			Content: zed,
		},
		{
			ID: "trae", Name: "Trae", Format: "json",
			Path:    "~/.trae/mcp.json",
			Note:    "或在 Trae 设置 → MCP → 添加 → 手动配置。",
			Content: stdioJSON,
		},
		{
			ID: "jetbrains", Name: "JetBrains AI", Format: "json",
			Path:    "Settings → Tools → AI Assistant → MCP",
			Note:    "粘贴 JSON 后点 OK。",
			Content: stdioJSON,
		},
		{
			ID: "cherry-studio", Name: "Cherry Studio", Format: "json",
			Path:    "设置 → MCP 服务器 → 添加服务器",
			Note:    "选「从 JSON 导入」后粘贴。",
			Content: stdioJSON,
		},
		{
			ID: "http", Name: "通用 HTTP 客户端", Format: "json",
			Path: "任意支持 streamable-http 的客户端",
			Note: "只能填 URL 的客户端用这个：必须带 Authorization，否则会被 401 拒绝；" +
				"App 重启后端口会变，需要重新复制端点。",
			Content: httpJSON,
		},
	}
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
