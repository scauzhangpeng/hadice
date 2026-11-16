package backend

// MCPService 是「MCP 服务」设置页的后端接口（Wails 绑定给前端调用）。
//
// 只做三件事：把内嵌 MCP 的状态/配置暴露给界面、把界面的修改写回 Store、
// 把审批裁决回传给挂起的工具调用。设备能力仍然全部由 mcpserver 包持有。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"Hadice/backend/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPService 是 v3 服务，供「MCP 服务」设置页使用。
type MCPService struct{}

// NewMCPService 创建 MCP 设置服务。
func NewMCPService() *MCPService { return &MCPService{} }

// MCPClientUsage 是按调用方聚合的使用统计（来自审计）。
type MCPClientUsage struct {
	Client string `json:"client"`
	Calls  int    `json:"calls"`
	Last   string `json:"last"`
}

// MCPOverview 是设置页首屏所需的全部数据。
type MCPOverview struct {
	Status       mcpserver.Status     `json:"status"`
	Settings     mcpserver.Settings   `json:"settings"`
	Templates    []mcpserver.Template `json:"templates"`
	BridgePath   string               `json:"bridgePath"`
	BridgeExists bool                 `json:"bridgeExists"`
	Clients      []MCPClientUsage     `json:"clients"`
}

// MCPCheckItem 是一项自检结果。
type MCPCheckItem struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// MCPSelfCheck 是一次自检的完整结果。
type MCPSelfCheck struct {
	OK    bool           `json:"ok"`
	Time  string         `json:"time"`
	Items []MCPCheckItem `json:"items"`
}

// GetMCPOverview 返回状态 + 设置 + 客户端模板。
func (s *MCPService) GetMCPOverview() (*MCPOverview, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	st := rt.Status()

	bridge := BridgeBinaryPath()
	_, statErr := os.Stat(bridge)

	return &MCPOverview{
		Status:       st,
		Settings:     rt.Store().Get(),
		Templates:    mcpserver.RenderTemplates(mcpserver.TemplateVars{Command: bridge, Endpoint: st.TCP, Token: st.Token}),
		BridgePath:   bridge,
		BridgeExists: statErr == nil,
		Clients:      aggregateClients(rt),
	}, nil
}

// GetMCPStatus 只返回状态（用于定时刷新）。
func (s *MCPService) GetMCPStatus() (*mcpserver.Status, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	st := rt.Status()
	return &st, nil
}

// GetMCPSettings 返回当前设置。
func (s *MCPService) GetMCPSettings() (*mcpserver.Settings, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	cur := rt.Store().Get()
	return &cur, nil
}

// UpdateMCPSettings 整体保存设置。
func (s *MCPService) UpdateMCPSettings(in mcpserver.Settings) (*mcpserver.Settings, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	next, uerr := rt.Store().Update(func(cur *mcpserver.Settings) error {
		*cur = in
		return nil
	})
	if uerr != nil {
		return nil, uerr
	}
	return &next, nil
}

// SetMCPToolEnabled 启用/停用单个工具。
func (s *MCPService) SetMCPToolEnabled(name string, enabled bool) (*mcpserver.Settings, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	next, uerr := rt.Store().Update(func(cur *mcpserver.Settings) error {
		out := make([]string, 0, len(cur.DisabledTools)+1)
		for _, d := range cur.DisabledTools {
			if d != name {
				out = append(out, d)
			}
		}
		if !enabled {
			out = append(out, name)
		}
		cur.DisabledTools = out
		return nil
	})
	if uerr != nil {
		return nil, uerr
	}
	return &next, nil
}

// SetMCPToolApproval 设置单个工具是否需要人工确认。
func (s *MCPService) SetMCPToolApproval(name string, required bool) (*mcpserver.Settings, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	next, uerr := rt.Store().Update(func(cur *mcpserver.Settings) error {
		out := make([]string, 0, len(cur.Approval.Tools)+1)
		for _, t := range cur.Approval.Tools {
			if t != name {
				out = append(out, t)
			}
		}
		if required {
			out = append(out, name)
		}
		cur.Approval.Tools = out
		return nil
	})
	if uerr != nil {
		return nil, uerr
	}
	return &next, nil
}

// ListMCPAudit 返回最近 n 条审计（读文件，重启 App 后仍可查看）。
func (s *MCPService) ListMCPAudit(limit int) ([]mcpserver.AuditEntry, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return rt.Auditor().Read(limit), nil
}

// ClearMCPAudit 清空审计文件。
func (s *MCPService) ClearMCPAudit() error {
	rt, err := mcpRuntime()
	if err != nil {
		return err
	}
	return rt.Auditor().Clear()
}

// ResolveMCPApproval 由前端模态框回传审批结果。
func (s *MCPService) ResolveMCPApproval(id string, allow bool, remember bool) error {
	rt, err := mcpRuntime()
	if err != nil {
		return err
	}
	return rt.Approver().Resolve(id, mcpserver.ApprovalDecision{Allow: allow, Remember: remember})
}

// GetMCPPendingApprovals 返回仍在等待的审批（页面刷新后补齐）。
func (s *MCPService) GetMCPPendingApprovals() ([]mcpserver.ApprovalRequest, error) {
	rt, err := mcpRuntime()
	if err != nil {
		return nil, err
	}
	return rt.Approver().Pending(), nil
}

// SelfCheckMCP 逐项检查 MCP 链路是否通（对应设置页的"重新检查"）。
func (s *MCPService) SelfCheckMCP() (*MCPSelfCheck, error) {
	out := &MCPSelfCheck{Time: time.Now().Format(time.RFC3339)}
	add := func(name string, ok bool, detail, hint string) {
		out.Items = append(out.Items, MCPCheckItem{Name: name, OK: ok, Detail: detail, Hint: hint})
	}

	rt, rtErr := mcpRuntime()
	if rtErr != nil {
		add("MCP 服务", false, rtErr.Error(), "确认 App 已启动；若设置了 HADICE_MCP_DISABLED=1 请先取消")
		out.OK = false
		return out, nil
	}

	st := rt.Status()
	add("MCP 服务", st.Running, fmt.Sprintf("%s（构建 %s，已运行 %ds）", st.Version, st.Build, st.UptimeSec),
		"在设置页重新启用，或重启 App")

	// discovery 文件：桥接进程靠它找 App。
	discPath := mcpserver.EndpointFilePath()
	if info, err := mcpserver.ReadEndpointFile(); err == nil {
		add("discovery 文件", true, fmt.Sprintf("%s（transport=%s, pid=%d）", discPath, info.Transport, info.PID), "")
	} else {
		add("discovery 文件", false, fmt.Sprintf("%s：%v", discPath, err),
			"App 正常启动会自动写入；若文件被沙箱/权限拦截，请把 App 装到 /Applications 后以正常方式启动")
	}

	// Unix socket：文件权限即鉴权。
	if st.Socket != "" {
		if fi, err := os.Stat(st.Socket); err == nil {
			ok := fi.Mode().Perm()&0o077 == 0
			add("Unix socket", ok, fmt.Sprintf("%s（权限 %s）", st.Socket, fi.Mode().Perm()),
				"权限应为 0600：仅当前用户可访问")
		} else {
			add("Unix socket", false, fmt.Sprintf("%s：%v", st.Socket, err), "重启 App 重新创建")
		}
	} else {
		add("Unix socket", false, "未启用", "在设置中开启（stdio 桥接默认走这条）")
	}

	// TCP 端点：只给 URL 型客户端用，必须带 token。
	if st.TCP != "" {
		info, _ := mcpserver.ReadEndpointFile()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		alive := info != nil && mcpserver.EndpointAlive(ctx, info)
		cancel()
		add("TCP 端点", alive, st.TCP, "端口可能被占用，或 App 正在重启")
		add("TCP 鉴权", st.Token != "", "已生成 token（写在本机 0600 文件中）", "token 为空表示任何网页都能调用，务必重新启用")
	} else {
		// 未启用 ≠ 故障：stdio 桥接已覆盖绝大多数客户端，TCP 只是给「只能填 URL」
		// 的客户端兜底。这里若记为失败，用户一关掉 TCP 自检就整体报红，属于误报。
		add("TCP 端点（可选）", true, "未启用 —— stdio 桥接已够用，仅 URL 型客户端需要",
			"在「传输与阈值」中开启")
	}

	// 桥接二进制：客户端配置里要填它。
	bridge := BridgeBinaryPath()
	bridgeOK := false
	if fi, err := os.Stat(bridge); err == nil {
		bridgeOK = true
		add("桥接二进制", true, fmt.Sprintf("%s（%d KB）", bridge, fi.Size()/1024), "")
	} else {
		add("桥接二进制", false, fmt.Sprintf("%s 不存在", bridge),
			"当前 App 包内未包含 hadice-mcp；用 build/scripts/package_mcp.sh 的 INJECT_APP=1 重新打包")
	}

	// 桥接握手：模拟 MCP 客户端拉起 hadice-mcp（stdio），完成 initialize + tools/list。
	// 这是 WorkBuddy / Codex 实际走的完整链路，比逐项静态检查更接近真实可用性。
	if bridgeOK {
		handshakeOK, detail, hint := bridgeHandshake(30 * time.Second)
		add("桥接握手", handshakeOK, detail, hint)
	}

	// 审计落盘：目录可写即可。
	auditPath := rt.Auditor().Path()
	if f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_ = f.Close()
		add("审计写入", true, auditPath, "")
	} else {
		add("审计写入", false, fmt.Sprintf("%s：%v", auditPath, err), "检查目录权限/磁盘空间")
	}

	out.OK = true
	for _, it := range out.Items {
		if !it.OK {
			out.OK = false
		}
	}
	return out, nil
}

// ClearMCPRemembered 清空"本次运行内记住允许"的审批。
func (s *MCPService) ClearMCPRemembered() error {
	rt, err := mcpRuntime()
	if err != nil {
		return err
	}
	rt.Approver().ClearRemembered()
	return nil
}

// bridgeHandshake 以"真实客户端"的方式拉起桥接二进制并完成一次 MCP 握手：
// stdio 连接 → initialize → tools/list。成功即证明 WorkBuddy/Codex 的接入链路
// 可用（discovery 发现 → socket 连接 → 工具清单下发，全链路验证）。
func bridgeHandshake(timeout time.Duration) (bool, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, BridgeBinaryPath())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "hadice-selfcheck", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		msg := stderr.String()
		if len(msg) > 200 {
			msg = msg[len(msg)-200:]
		}
		return false, fmt.Sprintf("握手失败：%v%s", err, stderrSuffix(msg)),
			"先确认 App 主窗口已启动；若 discovery 文件缺失，重启 App 再检查"
	}
	defer func() { _ = sess.Close() }()

	list, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return false, fmt.Sprintf("握手成功但工具清单获取失败：%v", err), "重启 App 后重新检查"
	}

	got := len(list.Tools)
	want := 0
	if rt, rtErr := mcpRuntime(); rtErr == nil {
		want = rt.Status().ToolCount
	}
	detail := fmt.Sprintf("握手成功，桥接端下发 %d 个工具", got)
	if want > 0 && got != want {
		detail += fmt.Sprintf("（App 当前 %d 个；客户端重连后清单会自动同步）", want)
	}
	return true, detail, ""
}

func stderrSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return "；桥接 stderr：" + msg
}

// BridgeBinaryPath 返回客户端要填的 hadice-mcp 路径（与 App 主程序同目录）。
func BridgeBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "hadice-mcp"
	}
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, "hadice-mcp")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// 开发态：App 从 bin/ 启动，桥也在 bin/ 下，路径一致；这里再兜一层仓库布局。
	if wd, werr := os.Getwd(); werr == nil {
		alt := filepath.Join(wd, "bin", "hadice-mcp")
		if _, serr := os.Stat(alt); serr == nil {
			return alt
		}
	}
	return candidate
}

func aggregateClients(rt *mcpserver.Runtime) []MCPClientUsage {
	entries := rt.Auditor().Read(200)
	type agg struct {
		calls int
		last  string
	}
	m := make(map[string]*agg)
	for _, e := range entries {
		name := e.Client
		if name == "" {
			name = "未知"
		}
		a, ok := m[name]
		if !ok {
			a = &agg{}
			m[name] = a
		}
		a.calls++
		if a.last == "" {
			a.last = e.Time
		}
	}
	out := make([]MCPClientUsage, 0, len(m))
	for name, a := range m {
		out = append(out, MCPClientUsage{Client: name, Calls: a.calls, Last: a.last})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Calls > out[j].Calls })
	return out
}
