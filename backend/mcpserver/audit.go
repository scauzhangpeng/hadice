package mcpserver

// 调用审计。
//
// 存储选择：**追加式 JSONL 文件**（<UserConfigDir>/hadice/mcp-audit.jsonl），
// 不使用 SQLite 等本地数据库，理由：
//   - 单写者、只追加，天然串行，不需要事务与索引；
//   - 崩溃安全：一行一条 JSON，断电最多丢最后一行，不会损坏历史；
//   - 可直接 `tail -f` / `grep` 排查，用户与支持人员无需装工具；
//   - 不引入 CGO/sqlite 依赖，打包体积与跨平台都不受影响。
//
// 代价是没有索引与聚合查询。审计的检索需求是"最近 N 条 + 按工具筛"，
// 顺序扫 5MB 文件（约 1~2 万条）在毫秒级，够用；等真的需要按月统计/长期留存
// 再迁 SQLite，届时只需替换本文件的读写实现。
//
// 轮转：单文件超过 auditMaxBytes 时改名为 .1（只保留一份历史），
// 避免无限增长又不做复杂的分段管理。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	auditFileEnv = "HADICE_MCP_AUDIT_FILE"
	// auditMaxBytes 单文件上限（约 1.5 万条，够回溯数天）。
	auditMaxBytes = 5 << 20
	// auditRingSize 内存里保留的最近记录条数（状态页/自检用，避免每次读盘）。
	auditRingSize = 200
	// auditArgsLimit / auditSummaryLimit 单条记录的参数与结果摘要长度上限。
	auditArgsLimit    = 600
	auditSummaryLimit = 600
)

// 审计判定结果。
const (
	DecisionOK             = "ok"
	DecisionError          = "error"
	DecisionDeniedPolicy   = "denied:policy"
	DecisionDeniedApproval = "denied:approval"
	DecisionTimeout        = "timeout"
	DecisionDisabled       = "denied:disabled"
)

// AuditEntry 是一条工具调用记录。
type AuditEntry struct {
	Time       string `json:"time"`
	Tool       string `json:"tool"`
	Client     string `json:"client,omitempty"`
	Session    string `json:"session,omitempty"`
	Args       string `json:"args,omitempty"`
	Decision   string `json:"decision"`
	Approval   string `json:"approval,omitempty"`
	DurationMS int64  `json:"durationMs"`
	IsError    bool   `json:"isError"`
	Summary    string `json:"summary,omitempty"`
}

// Auditor 负责审计写入与读取。
type Auditor struct {
	mu   sync.Mutex
	path string
	ring []AuditEntry
}

// AuditFilePath 返回审计文件路径（HADICE_MCP_AUDIT_FILE 优先）。
func AuditFilePath() string {
	if p := os.Getenv(auditFileEnv); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "hadice-mcp-audit.jsonl"
	}
	return filepath.Join(dir, "hadice", "mcp-audit.jsonl")
}

// NewAuditor 创建审计器（不创建文件，首次写入时才创建）。
func NewAuditor(path string) *Auditor { return &Auditor{path: path} }

// Path 返回审计文件路径。
func (a *Auditor) Path() string { return a.path }

// Append 追加一条记录；返回错误时调用方仍可继续（审计失败不应阻断工具调用）。
func (a *Auditor) Append(e AuditEntry) error {
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	e.Args = truncate(e.Args, auditArgsLimit)
	e.Summary = truncate(e.Summary, auditSummaryLimit)

	a.mu.Lock()
	a.ring = append(a.ring, e)
	if len(a.ring) > auditRingSize {
		a.ring = a.ring[len(a.ring)-auditRingSize:]
	}
	path := a.path
	a.mu.Unlock()

	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > auditMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Recent 返回内存里最近 n 条（新→旧），供状态页快速展示。
func (a *Auditor) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return tailOf(a.ring, n)
}

// Read 从文件读取最近 n 条（新→旧），用于重启后仍能查看历史。
func (a *Auditor) Read(n int) []AuditEntry {
	f, err := os.Open(a.path)
	if err != nil {
		return a.Recent(n)
	}
	defer f.Close()

	entries := make([]AuditEntry, 0, 256)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return a.Recent(n)
	}
	return tailOf(entries, n)
}

// Clear 清空审计（文件 + 内存）。
func (a *Auditor) Clear() error {
	a.mu.Lock()
	a.ring = nil
	path := a.path
	a.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(path + ".1")
	return nil
}

func tailOf(in []AuditEntry, n int) []AuditEntry {
	if n <= 0 || n > len(in) {
		n = len(in)
	}
	out := make([]AuditEntry, 0, n)
	for i := len(in) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, in[i])
	}
	return out
}

// truncate 按字符截断并标记，避免把超大参数/结果写进审计。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…(truncated)"
}
