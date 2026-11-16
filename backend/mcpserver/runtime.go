package mcpserver

import (
	"Hadice/backend/device"
	"strings"
	"sync"
	"time"
)

// Runtime 是 MCP 工具访问设备能力与本地策略的唯一入口。
//
// 它必须由 App 注入全局的 deviceManager（与 GUI 同一个实例），因此 MCP 与
// 界面共享同一份设备缓存、provider 与状态——工具结果与界面永远一致，App 在
// 管理器上积累的能力（设备事件、缓存、后续会话）桥接出去即可直接使用。
//
// 本包不自行创建 Manager：MCP 只作为 App 的能力出口存在，不承担独立运行的职责。
type Runtime struct {
	manager *device.Manager

	store    *Store
	auditor  *Auditor
	approver *Approver
	logs     *logHub

	startedAt time.Time

	mu           sync.RWMutex
	endpoints    Endpoints
	closeFn      func()
	callCounts   map[string]int
	recentErrors []AuditEntry

	// 设备级串行化：同一台设备上的工具调用排队执行。
	// 多个 MCP 客户端（如 WorkBuddy + Codex）同时操作同一台设备时，
	// install/clear_data 这类非幂等命令会互相打断，串行化后行为可预期。
	devMu    sync.Mutex
	devLocks map[string]*sync.Mutex
	sizes    map[string]screenSize
}

// NewRuntime 用 App 的设备管理器构建运行时（设置/审计/审批按默认路径初始化）。
func NewRuntime(mgr *device.Manager) *Runtime {
	if mgr == nil {
		panic("mcpserver: device manager must not be nil")
	}
	rt := &Runtime{
		manager:    mgr,
		store:      NewStore(),
		auditor:    NewAuditor(AuditFilePath()),
		approver:   NewApprover(),
		startedAt:  time.Now(),
		callCounts: make(map[string]int),
	}
	// 审批"记住允许"持久化：启动时恢复，勾选后落盘（mcp-settings.json）。
	rt.approver.SetPersist(func(list []string) {
		_, _ = rt.store.Update(func(s *Settings) error {
			s.Approval.Remembered = append([]string{}, list...)
			return nil
		})
	})
	rt.approver.LoadRemembered(rt.store.Get().Approval.Remembered)
	rt.logs = newLogHub(rt)
	SetDefault(rt)
	return rt
}

// Manager 返回设备管理器（设备枚举与平台路由的唯一入口）。
func (r *Runtime) Manager() *device.Manager { return r.manager }

// Store 返回设置容器（GUI 设置页与工具守卫共用）。
func (r *Runtime) Store() *Store { return r.store }

// Auditor 返回审计器。
func (r *Runtime) Auditor() *Auditor { return r.auditor }

// Approver 返回审批器。
func (r *Runtime) Approver() *Approver { return r.approver }

// SetEndpoints 记录实际监听端点（用于状态页展示与诊断）。
func (r *Runtime) SetEndpoints(eps Endpoints, closeFn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = eps
	r.closeFn = closeFn
}

// Endpoints 返回当前端点快照。
func (r *Runtime) Endpoints() Endpoints {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.endpoints
}

// Stop 停止监听（设置里关闭 MCP 时调用）。
func (r *Runtime) Stop() {
	r.mu.Lock()
	fn := r.closeFn
	r.closeFn = nil
	r.endpoints = Endpoints{}
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
	if r.logs != nil {
		r.logs.stopAll()
	}
}

// Logs 返回日志流管理器（MCP 资源读取用）。
func (r *Runtime) Logs() *logHub { return r.logs }

// PlatformFor 返回 connectKey 所属平台。
// 设备缓存通常由 list_devices 填充；若调用方未先调 list_devices 而直接调用
// 后续工具导致缓存未命中，这里刷新一次设备列表兜底，避免强依赖调用顺序。
func (r *Runtime) PlatformFor(connectKey string) device.Platform {
	p := r.manager.GetDevicePlatform(connectKey)
	if p == "" {
		_, _ = r.manager.ListAllDevices(false)
		p = r.manager.GetDevicePlatform(connectKey)
	}
	return p
}

// lockDevice 取设备级互斥锁并锁定，返回解锁函数。
//
// 锁按 connectKey 分片：不同设备互不影响，同一设备上的调用串行执行。
// connectKey 为空（如 list_devices）时不加锁，直接返回空操作。
func (r *Runtime) lockDevice(connectKey string) func() {
	connectKey = strings.TrimSpace(connectKey)
	if connectKey == "" {
		return func() {}
	}
	r.devMu.Lock()
	if r.devLocks == nil {
		r.devLocks = make(map[string]*sync.Mutex)
	}
	l, ok := r.devLocks[connectKey]
	if !ok {
		l = &sync.Mutex{}
		r.devLocks[connectKey] = l
	}
	r.devMu.Unlock()

	l.Lock()
	return l.Unlock
}

// record 写入一条审计并更新内存统计。
func (r *Runtime) record(e AuditEntry) {
	r.mu.Lock()
	r.callCounts[e.Tool]++
	if e.Decision != DecisionOK {
		r.recentErrors = append(r.recentErrors, e)
		if len(r.recentErrors) > 20 {
			r.recentErrors = r.recentErrors[len(r.recentErrors)-20:]
		}
	}
	r.mu.Unlock()

	if r.store.AuditEnabled() {
		_ = r.auditor.Append(e)
	}
}

// recentErrorEntries 返回最近失败/被拒的调用（新→旧）。
func (r *Runtime) recentErrorEntries() []AuditEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AuditEntry, 0, len(r.recentErrors))
	for i := len(r.recentErrors) - 1; i >= 0; i-- {
		out = append(out, r.recentErrors[i])
	}
	return out
}

// 默认运行时：供 GUI 服务读取状态（App 内只会有一个实例）。
var (
	defaultMu      sync.RWMutex
	defaultRuntime *Runtime
)

// SetDefault 记录全局运行时（NewRuntime 时自动调用）。
func SetDefault(rt *Runtime) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultRuntime = rt
}

// Default 返回全局运行时，未初始化时为 nil。
func Default() *Runtime {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultRuntime
}
