package mcpserver

// MCP resources：把"当前设备/服务状态"作为可读资源暴露。
//
// 工具是"动作"，resource 是"状态"。模型需要知道设备情况时可以直接读
// hadice://devices，而不必每次都先调 list_devices 再读返回——
// 上下文更短，也更符合客户端"先看资源再决定动作"的习惯。

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerResources 注册全部只读资源。
func (r *Runtime) registerResources(s *mcp.Server) {
	add := func(uri, name, title, desc string, gen func() (any, error)) {
		s.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        name,
			Title:       title,
			Description: desc,
			MIMEType:    "application/json",
		}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			v, err := gen()
			if err != nil {
				return nil, err
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      uri,
					MIMEType: "application/json",
					Text:     string(b),
				}},
			}, nil
		})
	}

	add("hadice://devices", "devices", "已连接设备",
		"当前已连接的安卓/鸿蒙设备列表（含 connectKey、型号、系统版本、连接方式）",
		func() (any, error) {
			devices, err := r.manager.ListAllDevices(false)
			if err != nil {
				return nil, err
			}
			return map[string]any{"count": len(devices), "devices": devices}, nil
		})

	add("hadice://server/status", "server-status", "MCP 服务状态",
		"服务版本、监听端点、工具启用情况与高危审批配置",
		func() (any, error) {
			st := r.Status()
			return map[string]any{
				"version":       st.Version,
				"build":         st.Build,
				"running":       st.Running,
				"enabled":       st.Enabled,
				"toolCount":     st.ToolCount,
				"tools":         st.Tools,
				"uptimeSeconds": st.UptimeSec,
			}, nil
		})

	add("hadice://audit/recent", "audit-recent", "最近调用审计",
		"最近 50 次工具调用（工具名、调用方、耗时、结果）",
		func() (any, error) {
			return map[string]any{"entries": r.auditor.Recent(50)}, nil
		})

	// 设备日志流：URI 模板按设备实例化，游标式增量读取。
	//   hadice://device/{key}/logcat/tail        最近 200 条快照 + nextCursor
	//   hadice://device/{key}/logcat/since/{seq} seq 之后的增量（最多 500 条）
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "hadice://device/{key}/logcat/tail",
		Name:        "device-logcat-tail",
		Title:       "设备日志快照",
		Description: "Android 设备最近 200 条 logcat（threadtime），返回 cursor 字段；" +
			"把 cursor 填入 hadice://device/{key}/logcat/since/{cursor} 可增量跟踪后续日志。仅支持 Android 设备。",
		MIMEType: "application/json",
	}, r.logResourceHandler)

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "hadice://device/{key}/logcat/since/{cursor}",
		Name:        "device-logcat-since",
		Title:       "设备日志增量",
		Description: "读取 Android 设备 cursor 序号之后的 logcat 增量（最多 500 条）并返回新 cursor；" +
			"循环读取即可跟踪日志流。truncated=true 表示中间日志因缓冲上限被丢弃。仅支持 Android 设备。",
		MIMEType: "application/json",
	}, r.logResourceHandler)
}

// logResourceHandler 统一处理两个日志资源模板（按 URI 分发）。
func (r *Runtime) logResourceHandler(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := ""
	if req != nil && req.Params != nil {
		uri = req.Params.URI
	}
	key, cursor, err := parseLogURI(uri)
	if err != nil {
		return nil, err
	}
	snap, err := r.logs.read(key, cursor, logTailDefault)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}
