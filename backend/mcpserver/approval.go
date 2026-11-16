package mcpserver

// 高危工具的二次确认（审批）。
//
// 为什么弹窗在 Hadice App 里，而不是 MCP 客户端里：
// MCP 协议里服务端不能"要求客户端弹窗"——只有 `elicitation/create` 这一种
// 反向请求，且必须客户端在 initialize 时声明 elicitation capability 才会生效，
// 目前 WorkBuddy / Codex 等客户端都不支持（SDK 里 Elicit 会直接返回错误）。
// 所以审批只能落回本机：工具调用在服务端挂起 → 通过 Wails 事件把请求推给
// Hadice 窗口 → 用户在窗口里点"允许/拒绝" → 服务端继续或返回拒绝。
//
// 默认拒绝原则：
//   - 无 GUI（headless / 事件发不出去）→ 直接拒绝，并提示去启动 App 界面；
//   - 超时 → 拒绝（不让调用无限挂住客户端）；
//   - 客户端断开（ctx 取消）→ 拒绝。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNoApprovalUI 表示当前没有可用的审批界面（App 窗口未就绪）。
var ErrNoApprovalUI = errors.New("没有可用的审批界面")

// ApprovalRequest 是一次待审批调用（会被推送到前端展示）。
type ApprovalRequest struct {
	ID         string `json:"id"`
	Tool       string `json:"tool"`
	Client     string `json:"client,omitempty"`
	Args       string `json:"args,omitempty"`
	CreatedAt  string `json:"createdAt"`
	TimeoutSec int    `json:"timeoutSec"`
}

// ApprovalDecision 是用户的裁决。
type ApprovalDecision struct {
	Allow    bool   `json:"allow"`
	Remember bool   `json:"remember"`
	Reason   string `json:"reason,omitempty"`
}

type pendingApproval struct {
	req ApprovalRequest
	ch  chan ApprovalDecision
}

// Approver 管理审批队列。
type Approver struct {
	mu       sync.Mutex
	pending  map[string]*pendingApproval
	order    []string
	remember map[string]bool
	notify   func(ApprovalRequest) bool
	// persist 把"记住允许"清单落盘（Runtime 注入，写 mcp-settings.json）。
	// 为 nil 时退化为仅内存记忆（重启 App 后失效）。
	persist func([]string)
	seq     int
}

// NewApprover 创建审批器。
func NewApprover() *Approver {
	return &Approver{
		pending:  make(map[string]*pendingApproval),
		remember: make(map[string]bool),
	}
}

// SetPersist 注入持久化回调（传 nil 关闭持久化）。回调在 Approver 自己的
// 锁之外调用，实现方需自行保证并发安全。
func (a *Approver) SetPersist(fn func([]string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.persist = fn
}

// LoadRemembered 启动时从持久层恢复"记住允许"的工具。
func (a *Approver) LoadRemembered(tools []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.remember = make(map[string]bool, len(tools))
	for _, t := range tools {
		if t != "" {
			a.remember[t] = true
		}
	}
}

// rememberList 返回当前记住清单的快照（调用方需持锁或复制后使用）。
func (a *Approver) rememberList() []string {
	out := make([]string, 0, len(a.remember))
	for t := range a.remember {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// persistRemembered 在锁外把当前清单交给持久层。
func (a *Approver) persistRemembered() {
	fn := a.persist
	if fn == nil {
		return
	}
	fn(a.rememberList())
}

// SetNotifier 注入"把审批请求推给 UI"的回调，返回 false 表示 UI 不可用。
func (a *Approver) SetNotifier(fn func(ApprovalRequest) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notify = fn
}

// Request 请求审批，阻塞直到用户裁决、超时或取消。
func (a *Approver) Request(ctx context.Context, tool, client, args string, timeout time.Duration) (ApprovalDecision, error) {
	a.mu.Lock()
	if a.remember[tool] {
		a.mu.Unlock()
		return ApprovalDecision{Allow: true, Reason: "本次运行内已记住允许"}, nil
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	req := ApprovalRequest{
		ID:         newApprovalID(),
		Tool:       tool,
		Client:     client,
		Args:       truncate(args, auditArgsLimit),
		CreatedAt:  time.Now().Format(time.RFC3339),
		TimeoutSec: int(timeout / time.Second),
	}
	p := &pendingApproval{req: req, ch: make(chan ApprovalDecision, 1)}
	a.pending[req.ID] = p
	a.order = append(a.order, req.ID)
	notify := a.notify
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, req.ID)
		for i, id := range a.order {
			if id == req.ID {
				a.order = append(a.order[:i], a.order[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
	}()

	if notify == nil || !notify(req) {
		return ApprovalDecision{}, ErrNoApprovalUI
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-p.ch:
		if d.Allow && d.Remember {
			a.mu.Lock()
			a.remember[tool] = true
			a.mu.Unlock()
			a.persistRemembered()
		}
		return d, nil
	case <-timer.C:
		return ApprovalDecision{Reason: fmt.Sprintf("等待超过 %d 秒未确认", int(timeout/time.Second))},
			fmt.Errorf("审批超时")
	case <-ctx.Done():
		return ApprovalDecision{Reason: "调用方已断开"}, ctx.Err()
	}
}

// Resolve 由 UI 调用，裁决某个待审批请求。
func (a *Approver) Resolve(id string, d ApprovalDecision) error {
	a.mu.Lock()
	p, ok := a.pending[id]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("审批请求不存在或已过期: %s", id)
	}
	select {
	case p.ch <- d:
		return nil
	default:
		// 已裁决过，重复点击忽略即可，不算错误。
		return nil
	}
}

// Pending 返回当前待审批队列（供 UI 刷新后补齐状态）。
func (a *Approver) Pending() []ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ApprovalRequest, 0, len(a.order))
	for _, id := range a.order {
		if p, ok := a.pending[id]; ok {
			out = append(out, p.req)
		}
	}
	return out
}

// Remembered 返回本次运行内"记住允许"的工具。
func (a *Approver) Remembered() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.remember))
	for t := range a.remember {
		out = append(out, t)
	}
	return out
}

// ClearRemembered 清空"记住允许"（内存与持久层同步清空）。
func (a *Approver) ClearRemembered() {
	a.mu.Lock()
	a.remember = make(map[string]bool)
	a.mu.Unlock()
	a.persistRemembered()
}

func newApprovalID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("apr-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
