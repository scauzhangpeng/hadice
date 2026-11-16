package mcpserver

// stdio 桥接：hadice-mcp 被 MCP 客户端拉起后唯一的工作方式。
//
//	发现运行中的 Hadice App（discovery 文件 + PID + healthz）
//	  → 连接 App 内嵌 MCP 端点（unix socket 优先，其次 TCP+token）
//	  → 把 App 暴露的工具动态注册到本地 stdio server，调用原样转发
//
// 这样 WorkBuddy、Codex 等多个客户端各自拉起一份 hadice-mcp，却共享同一个
// App 实例的设备状态与完整底座能力；App 后续新增工具也无需重建客户端配置。
//
// 本进程不直接操作设备（不加载 adb/hdc、不自建设备管理器）：能力全部来自 App。
//
// 端点按需解析：App 每次启动都会换端口，若转发失败会在下次调用时重新读取
// discovery 文件并重连，因此重装/重启 App 不必重启 MCP 客户端（工具清单仍是
// 首次连接时的快照，新增工具需客户端重连才会出现）。

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// bridgeRetryInterval 等待 App 启动时的探测间隔。
	bridgeRetryInterval = 300 * time.Millisecond
	// bridgeReconnectWait 单次调用转发失败后，重连时等待 App 就绪的时长
	// （覆盖"App 正在重启"这一常见场景）。
	bridgeReconnectWait = 3 * time.Second
)

// upstream 是到 App 内嵌 MCP 的转发通道，可重连。
type upstream struct {
	mu        sync.Mutex
	sess      *mcp.ClientSession
	ep        string
	tools     []*mcp.Tool
	resources []*mcp.Resource
}

// BridgeStdio 以桥接方式运行 stdio MCP 服务，阻塞直至 stdio 会话结束。
//
// wait 是等待 App 就绪的最长时间（客户端可能先于 App 启动，留出启动窗口）；
// 0 表示只探测一次。返回错误时调用方应退出并在 stderr 给出可操作的提示。
func BridgeStdio(ctx context.Context, wait time.Duration) error {
	up := &upstream{}
	if err := up.connect(ctx, wait); err != nil {
		up.close()
		return err
	}
	defer up.close()

	// 工具清单在连接时定型：工具名/schema 直接复用 App 的声明，避免两边维护两份定义。
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Title:   ServerTitle + "（桥接 Hadice App）",
		Version: ServerVersion,
	}, nil)
	for _, t := range up.tools {
		t := t
		s.AddTool(&mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: t.Annotations,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// 透传：参数与结果原样转发，App 侧负责输入校验与错误语义。
			return up.call(ctx, t.Name, req.Params.Arguments)
		})
	}

	// 资源同样原样透传（设备列表 / 服务状态 / 最近审计），
	// 不在桥接端重新实现内容，避免两份数据不一致。
	for _, res := range up.resources {
		res := res
		s.AddResource(&mcp.Resource{
			URI:         res.URI,
			Name:        res.Name,
			Title:       res.Title,
			Description: res.Description,
			MIMEType:    res.MIMEType,
		}, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return up.readResource(ctx, res.URI)
		})
	}

	log.Printf("[%s] 已桥接到 Hadice App %s（%d 个工具，%d 个资源，传输 %s）",
		ServerName, up.endpoint(), len(up.tools), len(up.resources), up.transport())
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Printf("[%s] stdio 会话结束: %v", ServerName, err)
	}
	return nil
}

// call 转发一次工具调用；失败时重新解析端点并重连，重试一次。
func (u *upstream) call(ctx context.Context, name string, args any) (*mcp.CallToolResult, error) {
	if sess := u.session(); sess != nil {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err == nil {
			return res, nil
		}
		log.Printf("[%s] 转发 %s 失败（%v），尝试重连 App", ServerName, name, err)
	}
	if err := u.connect(ctx, bridgeReconnectWait); err != nil {
		return nil, fmt.Errorf("与 Hadice App 的连接已断开（%v）。请确认 App 仍在运行，然后重试", err)
	}
	sess := u.session()
	if sess == nil {
		return nil, fmt.Errorf("重连 Hadice App 失败：没有可用会话")
	}
	return sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

// readResource 转发一次资源读取；失败时重连并重试一次。
func (u *upstream) readResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error) {
	if sess := u.session(); sess != nil {
		res, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err == nil {
			return res, nil
		}
		log.Printf("[%s] 读取资源 %s 失败（%v），尝试重连 App", ServerName, uri, err)
	}
	if err := u.connect(ctx, bridgeReconnectWait); err != nil {
		return nil, fmt.Errorf("与 Hadice App 的连接已断开（%v）。请确认 App 仍在运行，然后重试", err)
	}
	sess := u.session()
	if sess == nil {
		return nil, fmt.Errorf("重连 Hadice App 失败：没有可用会话")
	}
	return sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
}

// session 返回当前会话（可能为 nil），调用方直接使用，不加锁。
func (u *upstream) session() *mcp.ClientSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sess
}

func (u *upstream) endpoint() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ep
}

func (u *upstream) transport() string {
	ep := u.endpoint()
	if strings.HasPrefix(ep, "http://unix") {
		return "unix socket"
	}
	return "tcp"
}

// connect 重新发现 App 并建立会话，成功后替换旧会话。
func (u *upstream) connect(ctx context.Context, wait time.Duration) error {
	info, err := waitForApp(ctx, wait)
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name: ServerName + "-bridge", Version: ServerVersion,
	}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   MCPURL(info),
		HTTPClient: bridgeHTTPClient(info),
	}, nil)
	if err != nil {
		return fmt.Errorf("连接 Hadice App 的 MCP 端点失败（%s）：%w", MCPURL(info), err)
	}
	list, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = sess.Close()
		return fmt.Errorf("获取 Hadice App 工具清单失败：%w", err)
	}
	// 资源清单失败不影响工具使用（老版本 App 可能还没有资源）。
	var resList *mcp.ListResourcesResult
	if rl, rerr := sess.ListResources(ctx, &mcp.ListResourcesParams{}); rerr == nil {
		resList = rl
	}

	u.mu.Lock()
	old := u.sess
	u.sess, u.ep, u.tools = sess, MCPURL(info), list.Tools
	u.resources = nil
	if resList != nil {
		u.resources = resList.Resources
	}
	u.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (u *upstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sess != nil {
		_ = u.sess.Close()
		u.sess = nil
	}
}

// bridgeHTTPClient 在标准客户端上补一个 X-Hadice-Client 头，
// 让 App 侧审计能记录"是谁在调用"（MCP 协议本身不携带 clientInfo）。
func bridgeHTTPClient(info *EndpointInfo) *http.Client {
	c := ClientFor(info)
	if ht, ok := c.Transport.(*headerTransport); ok {
		if label := parentProcessName(); label != "" {
			ht.header.Set(ClientHeader, label)
		}
	}
	return c
}

// parentProcessName 取父进程名（WorkBuddy / Codex / code ...）。
// 拿不到就返回空，不影响功能。
func parentProcessName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", fmt.Sprint(os.Getppid())).Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}

// waitForApp 等待 App 内嵌端点就绪（discovery 文件可读 + PID 存活 + healthz 通过）。
func waitForApp(ctx context.Context, wait time.Duration) (*EndpointInfo, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		info, err := ReadEndpointFile()
		if err == nil && EndpointAlive(ctx, info) {
			return info, nil
		}
		if err == nil {
			err = fmt.Errorf("App 端点不可达: %s", MCPURL(info))
			_ = RemoveEndpointFile()
		}
		lastErr = err

		if wait <= 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(bridgeRetryInterval):
		}
	}
	return nil, fmt.Errorf("未发现运行中的 Hadice App（discovery 文件：%s，原因：%v）。"+
		"请先启动 Hadice App 再重连本 MCP 连接器；App 运行后端点会自动重新注册",
		EndpointFilePath(), lastErr)
}
