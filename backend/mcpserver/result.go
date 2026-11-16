package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// okJSON 将结果序列化为缩进 JSON 文本返回（对模型可读性最好）。
func okJSON(v any) (*mcp.CallToolResult, struct{}, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail("序列化结果失败: %v", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, struct{}{}, nil
}

// okText 返回纯文本结果（用于无数据可返回、但需给调用方可读说明的场景）。
func okText(format string, args ...any) (*mcp.CallToolResult, struct{}, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, struct{}{}, nil
}

// fail 返回工具级错误（IsError=true），由模型读取错误信息并决定下一步，
// 而不是让 JSON-RPC 层直接报错中断会话。
func fail(format string, args ...any) (*mcp.CallToolResult, struct{}, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}, struct{}{}, nil
}

func boolPtr(b bool) *bool { return &b }
