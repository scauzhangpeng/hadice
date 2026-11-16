package mcpserver

// 状态聚合：给 GUI 设置页与 MCP resource 提供"当前服务到底处于什么状态"。
//
// 这一段是自检能力的核心——之前排查连接问题只能翻 stderr 日志，
// 现在端点、工具开关、审批队列、最近错误、审计与设置文件路径都在一个结构里。

import (
	"runtime/debug"
	"sort"
	"time"
)

// toolInfo 是工具目录里的元信息（与注册处一一对应，仅供展示与策略判断）。
type toolInfo struct {
	Name     string
	Domain   string
	HighRisk bool
}

// 工具目录：新增工具时在这里补一行，设置页分组与高危标记即自动生效。
var toolCatalog = []toolInfo{
	{"list_devices", "设备", false},
	{"device_info", "设备", false},
	{"list_apps", "应用", false},
	{"launch_app", "应用", false},
	{"device_screenshot", "应用", false},
	{"device_logcat", "应用", false},
	{"install_app", "包管理", true},
	{"uninstall_app", "包管理", true},
	{"clear_app_data", "包管理", true},
	{"device_tap", "输入", false},
	{"device_swipe", "输入", false},
	{"device_back", "输入", false},
}

// ToolStatus 是单个工具在设置页中的展示项。
type ToolStatus struct {
	Name          string `json:"name"`
	Domain        string `json:"domain"`
	HighRisk      bool   `json:"highRisk"`
	Enabled       bool   `json:"enabled"`
	NeedsApproval bool   `json:"needsApproval"`
	Calls         int    `json:"calls"`
}

// Status 是设置页需要的全部运行时状态。
type Status struct {
	Running    bool              `json:"running"`
	Enabled    bool              `json:"enabled"`
	Version    string            `json:"version"`
	Build      string            `json:"build"`
	StartedAt  string            `json:"startedAt"`
	UptimeSec  int64             `json:"uptimeSec"`
	TCP        string            `json:"tcp,omitempty"`
	Socket     string            `json:"socket,omitempty"`
	Token      string            `json:"token,omitempty"`
	ToolCount  int               `json:"toolCount"`
	Tools      []ToolStatus      `json:"tools"`
	Pending    []ApprovalRequest `json:"pending"`
	Remembered []string          `json:"remembered"`

	AuditPath    string `json:"auditPath"`
	SettingsPath string `json:"settingsPath"`
	SocketNote   string `json:"socketNote,omitempty"`

	RecentErrors []AuditEntry `json:"recentErrors"`
	RecentCalls  []AuditEntry `json:"recentCalls"`
}

// Status 汇总当前运行时状态。
func (r *Runtime) Status() Status {
	s := r.store.Get()
	eps := r.Endpoints()
	r.mu.RLock()
	running := eps.TCP != "" || eps.Socket != ""
	r.mu.RUnlock()

	st := Status{
		Running:      running,
		Enabled:      s.Enabled,
		Version:      ServerVersion,
		Build:        BuildInfo(),
		StartedAt:    r.startedAt.Format(time.RFC3339),
		UptimeSec:    int64(time.Since(r.startedAt).Seconds()),
		TCP:          eps.TCP,
		Socket:       eps.Socket,
		Token:        eps.Token,
		AuditPath:    r.auditor.Path(),
		SettingsPath: r.store.Path(),
		Pending:      r.approver.Pending(),
		Remembered:   r.approver.Remembered(),
		RecentErrors: r.recentErrorEntries(),
		RecentCalls:  r.auditor.Recent(30),
	}

	r.mu.RLock()
	counts := make(map[string]int, len(r.callCounts))
	for k, v := range r.callCounts {
		counts[k] = v
	}
	r.mu.RUnlock()

	for _, t := range toolCatalog {
		st.Tools = append(st.Tools, ToolStatus{
			Name:          t.Name,
			Domain:        t.Domain,
			HighRisk:      t.HighRisk,
			Enabled:       toolEnabled(s, t.Name),
			NeedsApproval: toolNeedsApproval(s, t.Name),
			Calls:         counts[t.Name],
		})
	}
	sort.SliceStable(st.Tools, func(i, j int) bool { return st.Tools[i].Domain < st.Tools[j].Domain })
	st.ToolCount = len(st.Tools)
	return st
}

func toolEnabled(s Settings, name string) bool {
	if !s.Enabled {
		return false
	}
	for _, d := range s.DisabledTools {
		if d == name {
			return false
		}
	}
	return true
}

func toolNeedsApproval(s Settings, name string) bool {
	if !s.Approval.Enabled {
		return false
	}
	for _, t := range s.Approval.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// BuildInfo 返回构建版本信息（git commit 短哈希），用于确认"客户端连的是哪一版"。
func BuildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev := ""
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			rev = s.Value[:7]
		}
	}
	if rev == "" {
		return "dev"
	}
	return rev
}
