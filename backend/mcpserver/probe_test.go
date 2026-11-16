package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"Hadice/backend/adb"
	"Hadice/backend/device"
	"Hadice/backend/hdc"
)

// buildBinary 构建 cmd/hadice-mcp，保证测试对象与交付物是同一个二进制。
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := t.TempDir() + "/hadice-mcp"
	build := exec.Command("go", "build", "-o", bin, "./cmd/hadice-mcp")
	build.Dir = "../.." // 仓库根
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hadice-mcp failed: %v\n%s", err, out)
	}
	return bin
}

// newAppRuntime 构造与 App 等价的 Runtime（独立 Manager + 双平台 provider），
// 让工具在测试进程内执行。测试进程 cwd 是 backend/mcpserver，
// 因此显式把 adb/hdc 指向仓库 assets。
func newAppRuntime(t *testing.T) *Runtime {
	t.Helper()
	// 隔离用户真实配置与审计：测试不应读到本机 mcp-settings.json，
	// 也不该往用户的审计文件里写测试记录。
	dir := t.TempDir()
	t.Setenv(settingsFileEnv, filepath.Join(dir, "mcp-settings.json"))
	t.Setenv(auditFileEnv, filepath.Join(dir, "mcp-audit.jsonl"))
	if root, err := filepath.Abs("../.."); err == nil {
		binDir := filepath.Join(root, "assets", runtime.GOOS, runtime.GOARCH, "bin")
		if _, err := os.Stat(binDir); err == nil {
			adb.SetBinDir(binDir)
			hdc.SetBinDir(binDir)
		}
	}
	mgr := device.NewManager()
	mgr.RegisterProvider(hdc.NewProvider())
	mgr.RegisterProvider(adb.NewProvider())
	return NewRuntime(mgr)
}

// startFakeApp 在测试进程内启动"App 侧" MCP 服务（与 backend.Run 内嵌同一条路径），
// 并写入 discovery 文件，返回端点文件路径供 hadice-mcp 子进程发现。
func startFakeApp(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	epFile := filepath.Join(t.TempDir(), "mcp-endpoint.json")
	return epFile, startFakeAppAt(t, ctx, epFile)
}

// startFakeAppAt 在指定 endpoint 文件上启动一份"App 实例"（换端口后重写文件，
// 等价于 App 重启），返回关闭该实例的函数。
func startFakeAppAt(t *testing.T, ctx context.Context, epFile string) func() {
	t.Helper()
	t.Setenv(endpointFileEnv, epFile)

	var endpoint string
	closeFn, err := ServeHTTP(New(newAppRuntime(t)), "127.0.0.1:0", func(addr net.Addr) {
		host, port, _ := net.SplitHostPort(addr.String())
		endpoint = "http://" + net.JoinHostPort(host, port) + "/mcp"
	})
	if err != nil {
		t.Fatalf("start fake app server: %v", err)
	}
	t.Cleanup(closeFn)

	if err := WriteEndpointFile(EndpointInfo{
		Transport: TransportTCP,
		Endpoint:  endpoint,
		PID:       os.Getpid(),
		Version:   ServerVersion,
	}); err != nil {
		t.Fatalf("write endpoint file: %v", err)
	}
	t.Cleanup(func() { _ = RemoveEndpointFile() })

	// 等 healthz 就绪，避免子进程抢跑（真实 API 也是异步启动）。
	for i := 0; i < 50; i++ {
		info, rerr := ReadEndpointFile()
		if rerr == nil && EndpointAlive(ctx, info) {
			return closeFn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fake app endpoint not ready: %s", endpoint)
	return closeFn
}

// callListDevices 调用 list_devices 并断言返回结构（真机场景）。
// 返回第一台安卓设备的 connectKey（无设备时测试已被 Skip，不会走到返回）。
func callListDevices(t *testing.T, ctx context.Context, session *mcp.ClientSession) string {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_devices",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool list_devices failed: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("list_devices returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("list_devices returned non-text content: %T", res.Content[0])
	}
	t.Logf("list_devices output:\n%s", text.Text)

	// 无设备连接属正常环境状态，跳过设备断言而不是失败。
	if strings.Contains(text.Text, "未检测到已连接设备") {
		t.Skip("no device connected, skip device assertion")
	}
	if res.IsError {
		t.Skipf("device enumeration unavailable in this environment: %s", text.Text)
	}

	var devices []map[string]any
	if err := json.Unmarshal([]byte(text.Text), &devices); err != nil {
		t.Fatalf("list_devices output is not a device JSON array: %v", err)
	}
	if len(devices) == 0 {
		t.Fatalf("expected at least 1 device")
	}
	firstKey := ""
	for _, d := range devices {
		if d["connectKey"] == "" || d["platform"] == "" {
			t.Fatalf("device missing connectKey/platform: %v", d)
		}
		t.Logf("device: connectKey=%v platform=%v model=%v type=%v status=%v",
			d["connectKey"], d["platform"], d["model"], d["connectionType"], d["status"])
		if firstKey == "" && d["platform"] == "android" {
			firstKey, _ = d["connectKey"].(string)
		}
	}
	return firstKey
}

// callDeviceInfo 调用 device_info 并断言返回核心字段。
func callDeviceInfo(t *testing.T, ctx context.Context, session *mcp.ClientSession, connectKey string) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "device_info",
		Arguments: map[string]any{"connectKey": connectKey},
	})
	if err != nil {
		t.Fatalf("CallTool device_info failed: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("device_info returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("device_info returned non-text content: %T", res.Content[0])
	}
	t.Logf("device_info output:\n%s", text.Text)
	if res.IsError {
		t.Fatalf("device_info returned error: %s", text.Text)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(text.Text), &info); err != nil {
		t.Fatalf("device_info output is not a JSON object: %v", err)
	}
	if info["productName"] == "" || info["osVersion"] == "" {
		t.Fatalf("device_info missing productName/osVersion: %v", info)
	}
}

// TestMCPBridge 端到端验证唯一运行路径：
//  1. 测试进程内起一个"App 侧" MCP 服务（与 backend.Run 内嵌同一实现）；
//  2. 写入 discovery 文件；
//  3. 拉起 hadice-mcp 子进程（与 WorkBuddy 的 stdio 接入完全一致）→ 自动桥接；
//  4. 工具清单、调用链路经转发全部可用。
func TestMCPBridge(t *testing.T) {
	bin := buildBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	epFile, _ := startFakeApp(t, ctx)

	cmd := exec.Command(bin)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), endpointFileEnv+"="+epFile)
	cmd.Stderr = os.Stderr // 服务日志走 stderr（stdout 是 JSON-RPC 通道）
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-bridge", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer session.Close()

	// 桥接进程本身没有任何本地工具，能看到工具即证明透传生效。
	list, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	bridged := map[string]bool{}
	for _, tool := range list.Tools {
		t.Logf("bridged tool: %s", tool.Name)
		bridged[tool.Name] = true
	}
	for _, want := range []string{
		"list_devices", "device_info", "list_apps",
		"launch_app", "device_screenshot", "device_logcat",
		"install_app", "uninstall_app", "clear_app_data",
		"device_tap", "device_swipe", "device_back",
	} {
		if !bridged[want] {
			t.Fatalf("tool %s not bridged, tools: %d", want, len(list.Tools))
		}
	}

	if key := callListDevices(t, ctx, session); key != "" {
		callDeviceInfo(t, ctx, session, key)
	}
}

// TestMCPBridgeReconnect App 重启（端口变化）后，桥接进程的下一次调用应自动重连，
// 而不是要求客户端重启 MCP 连接。这是"App 重装/重启即整条会话失效"的回归测试。
func TestMCPBridgeReconnect(t *testing.T) {
	bin := buildBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	epFile, closeA := startFakeApp(t, ctx)

	cmd := exec.Command(bin)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), endpointFileEnv+"="+epFile)
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "probe-reconnect", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer session.Close()

	mustListDevices := func(stage string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_devices",
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatalf("%s: CallTool list_devices failed: %v", stage, err)
		}
		if len(res.Content) == 0 {
			t.Fatalf("%s: list_devices returned no content", stage)
		}
	}
	mustListDevices("首次连接")

	// 模拟 App 重启：旧实例下线，新实例换端口后重写 discovery 文件。
	closeA()
	startFakeAppAt(t, ctx, epFile)
	mustListDevices("App 重启后")
}

// TestMCPWithoutApp App 未运行时应快速失败并给出可操作的提示，
// 而不是静默降级为某种"半可用"状态。
func TestMCPWithoutApp(t *testing.T) {
	bin := buildBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-wait=0s")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(),
		endpointFileEnv+"="+filepath.Join(t.TempDir(), "none.json"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when App is not running, output:\n%s", out)
	}
	if !strings.Contains(string(out), "未发现运行中的 Hadice App") {
		t.Fatalf("expected actionable error message, got:\n%s", out)
	}
}
