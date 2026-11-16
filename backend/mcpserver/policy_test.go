package mcpserver

// 鉴权 / 设置 / 审批 / 审计 / 模板 的单元测试。
// 这些逻辑与设备无关，全部可在无手机环境下验证。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTCPAuthRejects(t *testing.T) {
	token := "secret-token"
	h := TCPAuth(token)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	cases := []struct {
		name   string
		host   string
		origin string
		auth   string
		path   string
		want   int
	}{
		{"正常", "127.0.0.1:1234", "", "Bearer " + token, "/mcp", 200},
		{"无 token", "127.0.0.1:1234", "", "", "/mcp", 401},
		{"错误 token", "127.0.0.1:1234", "", "Bearer nope", "/mcp", 401},
		{"DNS rebinding", "evil.com:1234", "", "Bearer " + token, "/mcp", 403},
		{"跨站来源", "127.0.0.1:1234", "https://evil.com", "Bearer " + token, "/mcp", 403},
		{"回环来源", "127.0.0.1:1234", "http://127.0.0.1:5173", "Bearer " + token, "/mcp", 200},
		{"健康检查免 token", "127.0.0.1:1234", "", "", "/healthz", 200},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, nil)
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if c.want == 0 {
				if rec.Code != 200 {
					t.Fatalf("custom token header should pass, got %d", rec.Code)
				}
				return
			}
			if rec.Code != c.want {
				t.Fatalf("want %d got %d (%s)", c.want, rec.Code, rec.Body.String())
			}
		})
	}

	// X-Hadice-Token 头等价于 Bearer。
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Host = "localhost:1234"
	req.Header.Set(TokenHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("X-Hadice-Token should pass, got %d", rec.Code)
	}
}

func TestSettingsPersistAndPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	st := OpenStore(path)

	if !st.ToolEnabled("list_devices") {
		t.Fatal("list_devices should be enabled by default")
	}
	if !st.NeedsApproval("install_app") {
		t.Fatal("install_app should require approval by default")
	}

	if _, err := st.Update(func(s *Settings) error {
		s.DisabledTools = append(s.DisabledTools, "device_screenshot")
		s.Approval.Tools = []string{"uninstall_app"}
		s.Approval.TimeoutSec = 1 // 会被夹到下限 5
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// 重新打开应读到同样的值（落盘正确）。
	re := OpenStore(path)
	if re.ToolEnabled("device_screenshot") {
		t.Fatal("device_screenshot should stay disabled after reload")
	}
	if re.NeedsApproval("install_app") {
		t.Fatal("install_app approval should be cleared after update")
	}
	if !re.NeedsApproval("uninstall_app") {
		t.Fatal("uninstall_app should require approval")
	}
	if got := re.ApprovalTimeout(); got != 5 {
		t.Fatalf("timeout should clamp to 5, got %d", got)
	}

	// 整体停用后所有工具都不可用。
	if _, err := st.Update(func(s *Settings) error { s.Enabled = false; return nil }); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if st.ToolEnabled("list_devices") {
		t.Fatal("all tools should be disabled when service disabled")
	}
}

func TestApprovalApproveAndTimeout(t *testing.T) {
	ap := NewApprover()

	// 1) UI 不可用 → 立即拒绝。
	if _, err := ap.Request(context.Background(), "install_app", "test", "{}", time.Second); err != ErrNoApprovalUI {
		t.Fatalf("want ErrNoApprovalUI, got %v", err)
	}

	// 2) 批准。
	ap.SetNotifier(func(r ApprovalRequest) bool {
		go func() { _ = ap.Resolve(r.ID, ApprovalDecision{Allow: true}) }()
		return true
	})
	dec, err := ap.Request(context.Background(), "install_app", "test", "{}", 3*time.Second)
	if err != nil || !dec.Allow {
		t.Fatalf("want approved, got %+v err=%v", dec, err)
	}

	// 3) 超时 → 拒绝。
	ap.SetNotifier(func(ApprovalRequest) bool { return true })
	dec, err = ap.Request(context.Background(), "uninstall_app", "test", "{}", 200*time.Millisecond)
	if err == nil || dec.Allow {
		t.Fatalf("want timeout denial, got %+v err=%v", dec, err)
	}
	if len(ap.Pending()) != 0 {
		t.Fatalf("pending should be empty, got %d", len(ap.Pending()))
	}
}

func TestAuditAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := NewAuditor(path)

	for i := 0; i < 3; i++ {
		if err := a.Append(AuditEntry{Tool: "list_devices", Decision: DecisionOK}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := a.Append(AuditEntry{Tool: "install_app", Decision: DecisionDeniedApproval,
		Args: strings.Repeat("x", auditArgsLimit+50)}); err != nil {
		t.Fatalf("append big: %v", err)
	}

	got := a.Read(10)
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d", len(got))
	}
	if got[0].Tool != "install_app" {
		t.Fatalf("newest first expected, got %q", got[0].Tool)
	}
	if len(got[0].Args) > auditArgsLimit+20 {
		t.Fatalf("args should be truncated, len=%d", len(got[0].Args))
	}

	// 重新打开（模拟重启）仍能读到历史。
	if re := (&Auditor{path: path}).Read(2); len(re) != 2 {
		t.Fatalf("reload should read 2 entries, got %d", len(re))
	}

	if err := a.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("audit file should be removed after clear")
	}
}

func TestRenderTemplates(t *testing.T) {
	ts := RenderTemplates(TemplateVars{
		Command:  "/Applications/Hadice.app/Contents/MacOS/hadice-mcp",
		Endpoint: "http://127.0.0.1:5000/mcp",
		Token:    "abc123",
	})
	if len(ts) < 15 {
		t.Fatalf("want at least 15 templates, got %d", len(ts))
	}
	for _, tp := range ts {
		if tp.ID == "" || tp.Name == "" || tp.Content == "" {
			t.Fatalf("template missing fields: %+v", tp)
		}
		if !strings.Contains(tp.Content, "hadice") && tp.ID != "http" {
			t.Fatalf("template %s should reference hadice bridge: %s", tp.ID, tp.Content)
		}
		if tp.Format == "json" {
			var v any
			if err := json.Unmarshal([]byte(tp.Content), &v); err != nil {
				t.Fatalf("template %s is not valid JSON: %v\n%s", tp.ID, err, tp.Content)
			}
		}
	}
	// HTTP 模板必须带 token，否则会被 401 拒绝。
	var httpTpl Template
	for _, tp := range ts {
		if tp.ID == "http" {
			httpTpl = tp
		}
	}
	if !strings.Contains(httpTpl.Content, "Bearer abc123") {
		t.Fatalf("http template missing token: %s", httpTpl.Content)
	}
}

// TestUnixSocketEndToEnd 验证 Unix socket 传输可用且无需 token，
// 并确认工具清单仍然完整（桥接默认走这条路）。
func TestUnixSocketEndToEnd(t *testing.T) {
	ctx := context.Background()
	rt := newAppRuntime(t)
	socket := filepath.Join(t.TempDir(), "mcp.sock")

	eps, closeFn, err := Serve(New(rt), ServeOptions{SocketPath: socket})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(closeFn)

	if eps.Socket != socket {
		t.Fatalf("socket endpoint not set: %+v", eps)
	}
	if fi, err := os.Stat(socket); err != nil {
		t.Fatalf("socket file missing: %v", err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket must be 0600, got %s", fi.Mode().Perm())
	}

	info := &EndpointInfo{Transport: TransportUnix, Socket: socket}
	if !EndpointAlive(ctx, info) {
		t.Fatal("healthz over unix socket should pass")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "unix-test", Version: "0.0.1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   MCPURL(info),
		HTTPClient: ClientFor(info),
	}, nil)
	if err != nil {
		t.Fatalf("connect over socket: %v", err)
	}
	defer sess.Close()

	list, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(list.Tools) != len(toolCatalog) {
		t.Fatalf("want %d tools, got %d", len(toolCatalog), len(list.Tools))
	}
}

// TestGuardDeniesDisabledTool 验证停用的工具调用会被拒绝并留下审计。
func TestGuardDeniesDisabledTool(t *testing.T) {
	ctx := context.Background()
	rt := newAppRuntime(t)
	if _, err := rt.Store().Update(func(s *Settings) error {
		s.DisabledTools = []string{"device_screenshot"}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "guard-test", Version: "0.0.1"}, nil)
	srv := New(rt)
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "device_screenshot",
		Arguments: map[string]any{"connectKey": "x"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("disabled tool should return error result, got %+v", res)
	}
	entries := rt.Auditor().Recent(5)
	if len(entries) == 0 || entries[0].Decision != DecisionDeniedPolicy {
		t.Fatalf("expected denied:policy audit, got %+v", entries)
	}
}
