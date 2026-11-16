package mcpserver

// 工具守卫：所有工具统一经过 registerTool 注册，从而在一个地方完成
//
//	策略检查（是否被停用）→ 高危审批 → 执行 → 审计
//
// 各工具实现本身不需要关心这些横切逻辑，新增工具只要用 registerTool 注册即自动获得。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTool 注册工具并套上守卫。
func registerTool[In any](rt *Runtime, s *mcp.Server, tool *mcp.Tool,
	h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, struct{}, error)) {
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, struct{}, error) {
		return invoke(rt, ctx, req, tool, in, h)
	})
}

// invoke 是守卫主体（方法不能带类型参数，所以写成普通函数）。
func invoke[In any](r *Runtime, ctx context.Context, req *mcp.CallToolRequest, tool *mcp.Tool, in In,
	h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, struct{}, error),
) (*mcp.CallToolResult, struct{}, error) {
	start := time.Now()
	entry := AuditEntry{
		Time:    start.Format(time.RFC3339),
		Tool:    tool.Name,
		Client:  callClient(req),
		Session: callSession(req),
		Args:    compactJSON(in),
	}

	// 1) 策略：整体开关 + 单工具开关。
	if !r.store.Get().Enabled {
		entry.Decision = DecisionDisabled
		entry.Summary = "MCP 服务已在 Hadice 设置中停用"
		entry.DurationMS = time.Since(start).Milliseconds()
		r.record(entry)
		return fail("MCP 服务已在 Hadice 的「MCP 服务」设置页停用，所有工具调用被拒绝。" +
			"请在 Hadice App 中重新启用后再试。")
	}
	if !r.store.ToolEnabled(tool.Name) {
		entry.Decision = DecisionDeniedPolicy
		entry.Summary = "工具已停用"
		entry.DurationMS = time.Since(start).Milliseconds()
		r.record(entry)
		return fail("工具 %s 已在 Hadice 的「MCP 服务」设置页停用。若确需使用，请在 App 中打开该工具后重试。", tool.Name)
	}

	// 1.5) 参数前置校验：把「明显不可能成功」的调用挡在审批之前。
	// 否则用户点了"允许"，才被告知包路径是 .apk（鸿蒙需要 .hap）——白等一轮。
	if err := precheckArgs(r, tool.Name, in); err != nil {
		entry.Decision = DecisionError
		entry.IsError = true
		entry.Summary = err.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		r.record(entry)
		return fail("%v", err)
	}

	// 2) 高危审批（默认拒绝原则见 approval.go）。
	if r.store.NeedsApproval(tool.Name) {
		timeout := time.Duration(r.store.ApprovalTimeout()) * time.Second
		dec, err := r.approver.Request(ctx, tool.Name, entry.Client, entry.Args, timeout)
		switch {
		case err == ErrNoApprovalUI:
			entry.Decision = DecisionDeniedApproval
			entry.Summary = "无审批界面"
			entry.DurationMS = time.Since(start).Milliseconds()
			r.record(entry)
			return fail("工具 %s 需要人工确认，但当前没有可用的审批界面（Hadice 窗口未就绪）。"+
				"请先打开 Hadice App 主窗口，再重试本次调用。", tool.Name)
		case err != nil && !dec.Allow:
			entry.Decision = DecisionTimeout
			entry.Summary = dec.Reason
			entry.DurationMS = time.Since(start).Milliseconds()
			r.record(entry)
			return fail("工具 %s 未获批准（%s）。已按拒绝处理，设备状态未改变。", tool.Name, dec.Reason)
		case !dec.Allow:
			entry.Decision = DecisionDeniedApproval
			entry.Summary = "用户拒绝"
			if dec.Reason != "" {
				entry.Summary = "用户拒绝：" + dec.Reason
			}
			entry.DurationMS = time.Since(start).Milliseconds()
			r.record(entry)
			return fail("用户在 Hadice 中拒绝了 %s 的执行，设备状态未改变。"+
				"如属误操作，可让用户在 App 中重新确认后再次调用。", tool.Name)
		default:
			entry.Approval = "approved"
			if dec.Reason != "" {
				entry.Approval = "approved (" + dec.Reason + ")"
			}
		}
	}

	// 3) 执行。同一台设备上的调用串行执行，避免多客户端互相打断。
	unlock := r.lockDevice(callConnectKey(in))
	defer unlock()
	res, out, err := h(ctx, req, in)
	entry.DurationMS = time.Since(start).Milliseconds()
	switch {
	case err != nil:
		entry.Decision = DecisionError
		entry.IsError = true
		entry.Summary = err.Error()
	case res != nil && res.IsError:
		entry.Decision = DecisionError
		entry.IsError = true
		entry.Summary = firstText(res)
	default:
		entry.Decision = DecisionOK
		entry.Summary = firstText(res)
	}
	r.record(entry)
	return res, out, err
}

// callConnectKey 从入参中取 connectKey（用于设备级串行化；取不到返回空）。
func callConnectKey(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	s, _ := m["connectKey"].(string)
	return strings.TrimSpace(s)
}

// precheckArgs 在弹审批之前校验参数，把必然失败的调用提前挡掉。
//
// 目前只覆盖 install_app：包路径/扩展名与设备平台是否匹配，纯本地判断、
// 不触碰设备，因此放在审批之前不会引入副作用。
func precheckArgs(r *Runtime, tool string, in any) error {
	if tool != "install_app" {
		return nil
	}
	var args installAppIn
	if b, err := json.Marshal(in); err == nil {
		_ = json.Unmarshal(b, &args)
	}
	p := r.PlatformFor(args.ConnectKey)
	if p == "" {
		return nil // 设备不存在时的提示由 handler 给（那里信息更全）
	}
	_, _, err := normalizePackagePath(p, args.ApkPath)
	return err
}

// compactJSON 把入参压成一行，用于审计与审批展示。
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// firstText 取结果的第一段文本作为审计摘要。
func firstText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return fmt.Sprintf("<%d 个非文本内容>", len(res.Content))
}

// callClient 取调用方标识（由桥接进程写入 X-Hadice-Client，值为父进程名）。
func callClient(req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		if v := req.Extra.Header.Get(ClientHeader); v != "" {
			return truncate(v, 64)
		}
	}
	return "本地"
}

// callSession 取会话 ID（MCP 未在调用中携带 clientInfo，只能定位到会话）。
func callSession(req *mcp.CallToolRequest) string {
	if req != nil && req.Session != nil {
		id := req.Session.ID()
		if len(id) > 12 {
			id = id[:12]
		}
		return id
	}
	return ""
}
