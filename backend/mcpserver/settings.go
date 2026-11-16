package mcpserver

// 服务设置：持久化在用户配置目录，供 GUI 设置页读取与修改。
//
// 存储格式是普通 JSON（<UserConfigDir>/hadice/mcp-settings.json），不引入数据库：
// 只有一份、单写者、结构简单，文件足够且便于用户手动排查。
// 每次工具调用都会读一次内存快照，所以开关改动立即生效，不需要重启 App
// （传输层开关除外——监听器已经建好了，需要重启 App 才能换）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// settingsFileEnv 允许测试与多实例场景覆盖设置文件位置。
const settingsFileEnv = "HADICE_MCP_SETTINGS_FILE"

// HighRiskTools 是需要人工确认的高危工具：都会改变设备上的应用状态，
// 且无法撤销（卸载/清数据）或可能装错包（安装）。
var HighRiskTools = []string{"install_app", "uninstall_app", "clear_app_data"}

// TransportSettings 控制 MCP 端点的暴露方式。
type TransportSettings struct {
	// TCP 是否监听 127.0.0.1 的 TCP 端口（HTTP 客户端用，需要 token 鉴权）。
	TCP bool `json:"tcp"`
	// UnixSocket 是否监听本机 Unix domain socket（stdio 桥默认走这条，靠文件权限鉴权）。
	UnixSocket bool `json:"unixSocket"`
	// FixedPort 固定 TCP 端口，0 表示由内核随机分配（推荐，避免端口冲突）。
	FixedPort int `json:"fixedPort"`
}

// ApprovalSettings 控制高危工具的二次确认。
type ApprovalSettings struct {
	// Enabled 是否开启审批；关闭后高危工具直接执行（不建议）。
	Enabled bool `json:"enabled"`
	// Tools 需要审批的工具名；空数组表示一个都不需要。
	Tools []string `json:"tools"`
	// TimeoutSec 单次审批等待时长，超时按拒绝处理。
	TimeoutSec int `json:"timeoutSec"`
	// Remembered 用户在弹窗里勾选"记住允许"的工具（持久化，重启 App 后仍生效）。
	// 与 Tools 分开存：记住的只是"不再询问"，不是"不再需要审批"——
	// 把工具从 Tools 移除才是彻底免审批。
	Remembered []string `json:"remembered"`
}

// AuditSettings 控制调用审计。
type AuditSettings struct {
	// Enabled 是否记录工具调用审计。
	Enabled bool `json:"enabled"`
}

// Settings 是 MCP 服务的完整可调项。
type Settings struct {
	// Enabled 关闭后所有工具调用被拒绝（监听器仍保留，重启 App 才会真正停掉）。
	Enabled bool `json:"enabled"`
	// Transport 传输层配置（改动需重启 App 生效）。
	Transport TransportSettings `json:"transport"`
	// DisabledTools 被停用的工具名：不出现在新会话的工具清单里，调用也会被拒绝。
	DisabledTools []string `json:"disabledTools"`
	// Approval 高危工具审批配置。
	Approval ApprovalSettings `json:"approval"`
	// Audit 审计配置。
	Audit AuditSettings `json:"audit"`
}

// DefaultSettings 返回出厂默认值：全开、双传输、高危工具走审批。
func DefaultSettings() Settings {
	return Settings{
		Enabled: true,
		Transport: TransportSettings{
			TCP:        true,
			UnixSocket: true,
			FixedPort:  0,
		},
		DisabledTools: []string{},
		Approval: ApprovalSettings{
			Enabled:    true,
			Tools:      append([]string{}, HighRiskTools...),
			TimeoutSec: 60,
		},
		Audit: AuditSettings{Enabled: true},
	}
}

// SettingsFilePath 返回设置文件路径（HADICE_MCP_SETTINGS_FILE 优先）。
func SettingsFilePath() string {
	if p := os.Getenv(settingsFileEnv); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "hadice-mcp-settings.json"
	}
	return filepath.Join(dir, "hadice", "mcp-settings.json")
}

// Store 是设置的并发安全容器：内存快照 + 原子落盘 + 变更订阅。
type Store struct {
	mu        sync.RWMutex
	path      string
	cur       Settings
	listeners []func(Settings)
}

// OpenStore 载入设置（文件不存在或损坏时用默认值，不阻塞启动）。
func OpenStore(path string) *Store {
	s := &Store{path: path, cur: DefaultSettings()}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var loaded Settings
	if err := json.Unmarshal(b, &loaded); err != nil {
		// 损坏的配置不该让功能整体不可用：回退默认值，等用户下次保存时覆盖。
		return s
	}
	s.cur = normalize(loaded)
	return s
}

// NewStore 使用默认路径打开设置。
func NewStore() *Store { return OpenStore(SettingsFilePath()) }

// Get 返回当前设置的副本（调用方可安全修改）。
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cur
	out.DisabledTools = append([]string{}, s.cur.DisabledTools...)
	out.Approval.Tools = append([]string{}, s.cur.Approval.Tools...)
	out.Approval.Remembered = append([]string{}, s.cur.Approval.Remembered...)
	return out
}

// Update 修改设置并落盘，成功后通知订阅者。
// fn 返回错误时不写入、不通知。
func (s *Store) Update(fn func(*Settings) error) (Settings, error) {
	s.mu.Lock()
	next := s.cur
	next.DisabledTools = append([]string{}, s.cur.DisabledTools...)
	next.Approval.Tools = append([]string{}, s.cur.Approval.Tools...)
	next.Approval.Remembered = append([]string{}, s.cur.Approval.Remembered...)
	if fn != nil {
		if err := fn(&next); err != nil {
			s.mu.Unlock()
			return s.Get(), err
		}
	}
	next = normalize(next)
	if err := writeJSONAtomic(s.path, next); err != nil {
		s.mu.Unlock()
		return s.Get(), err
	}
	s.cur = next
	listeners := append([]func(Settings){}, s.listeners...)
	s.mu.Unlock()

	snapshot := s.Get()
	for _, fn := range listeners {
		fn(snapshot)
	}
	return snapshot, nil
}

// Subscribe 注册变更回调（回调在 Update 返回前同步执行，勿做阻塞操作）。
func (s *Store) Subscribe(fn func(Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// ToolEnabled 判断工具是否可用。
func (s *Store) ToolEnabled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cur.Enabled {
		return false
	}
	for _, d := range s.cur.DisabledTools {
		if d == name {
			return false
		}
	}
	return true
}

// NeedsApproval 判断工具是否需要人工确认。
func (s *Store) NeedsApproval(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cur.Approval.Enabled {
		return false
	}
	for _, t := range s.cur.Approval.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// ApprovalTimeout 返回审批等待时长（带上下限兜底）。
func (s *Store) ApprovalTimeout() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clampTimeout(s.cur.Approval.TimeoutSec)
}

// AuditEnabled 判断是否记录审计。
func (s *Store) AuditEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Audit.Enabled
}

// Path 返回设置文件路径（GUI 上展示，便于用户手动编辑）。
func (s *Store) Path() string { return s.path }

// normalize 修正越界值，避免手改配置把服务弄坏。
func normalize(in Settings) Settings {
	if !in.Transport.TCP && !in.Transport.UnixSocket {
		// 两个传输都关掉等于自断通道，至少保留 Unix socket（仅本机可访问）。
		in.Transport.UnixSocket = true
	}
	if in.Transport.FixedPort != 0 && (in.Transport.FixedPort < 1024 || in.Transport.FixedPort > 65535) {
		in.Transport.FixedPort = 0
	}
	in.Approval.TimeoutSec = clampTimeout(in.Approval.TimeoutSec)
	if in.Approval.Tools == nil {
		in.Approval.Tools = append([]string{}, HighRiskTools...)
	}
	if in.DisabledTools == nil {
		in.DisabledTools = []string{}
	}
	// 去重，保持稳定顺序。
	in.DisabledTools = uniqueStrings(in.DisabledTools)
	in.Approval.Tools = uniqueStrings(in.Approval.Tools)
	if in.Approval.Remembered == nil {
		in.Approval.Remembered = []string{}
	}
	in.Approval.Remembered = uniqueStrings(in.Approval.Remembered)
	return in
}

func clampTimeout(sec int) int {
	if sec == 0 {
		return 60
	}
	if sec < 5 {
		return 5
	}
	if sec > 600 {
		return 600
	}
	return sec
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// writeJSONAtomic 原子写 JSON（临时文件 + rename），避免读到半截内容。
func writeJSONAtomic(path string, v any) error {
	if path == "" {
		return errors.New("empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
