package mcpserver

// discovery 机制：Hadice App 内嵌 MCP 服务启动后，把端点信息写入固定的
// discovery 文件；hadice-mcp 桥接进程据此发现 App，无需固定端口、无需用户配置。
// App 异常退出（未清理文件）由 PID 存活检查兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 传输方式取值。
const (
	TransportUnix = "unix"
	TransportTCP  = "tcp"
)

// EndpointInfo 是 discovery 文件的内容：App 内嵌 MCP 端点的发现信息。
type EndpointInfo struct {
	// Transport 传输方式：unix（推荐，文件权限即鉴权）或 tcp。
	Transport string `json:"transport"`
	// Endpoint TCP 端点，如 http://127.0.0.1:52341/mcp（transport=tcp 时必填）。
	Endpoint string `json:"endpoint,omitempty"`
	// Socket Unix socket 路径（transport=unix 时必填）。
	Socket string `json:"socket,omitempty"`
	// Token TCP 端点鉴权 token。
	Token string `json:"token,omitempty"`
	// PID 写入方（Hadice App）进程号，用于快速失效判断。
	PID int `json:"pid"`
	// Version MCP 服务版本。
	Version string `json:"version"`
	// Build 构建 commit 短哈希，用于确认客户端连的是哪一版。
	Build string `json:"build,omitempty"`
	// StartedAt 写入时间（调试用）。
	StartedAt string `json:"startedAt"`
}

// endpointFileEnv 允许测试与多实例场景覆盖 discovery 文件位置。
const endpointFileEnv = "HADICE_MCP_ENDPOINT_FILE"

// EndpointFilePath 返回 discovery 文件路径。
// 优先级：HADICE_MCP_ENDPOINT_FILE 环境变量 > <UserConfigDir>/hadice/mcp-endpoint.json
// （macOS 即 ~/Library/Application Support/hadice/mcp-endpoint.json）。
func EndpointFilePath() string {
	if p := os.Getenv(endpointFileEnv); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		// 极少见：拿不到用户配置目录。退化为当前目录，服务不因此失败。
		return "hadice-mcp-endpoint.json"
	}
	return filepath.Join(dir, "hadice", "mcp-endpoint.json")
}

// WriteEndpointFile 原子写入 discovery 文件（临时文件 + rename，避免客户端读到半截 JSON）。
// 文件权限 0600：里面的 token 属于凭据，不能让同机其它用户读取。
func WriteEndpointFile(info EndpointInfo) error {
	p := EndpointFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if info.Transport == "" {
		if info.Socket != "" {
			info.Transport = TransportUnix
		} else {
			info.Transport = TransportTCP
		}
	}
	if info.Version == "" {
		info.Version = ServerVersion
	}
	if info.Build == "" {
		info.Build = BuildInfo()
	}
	if info.StartedAt == "" {
		info.StartedAt = time.Now().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RemoveEndpointFile 删除 discovery 文件（App 正常退出时清理；文件不存在不算错误）。
func RemoveEndpointFile() error {
	if err := os.Remove(EndpointFilePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadEndpointFile 读取 discovery 文件。
// 文件不存在、JSON 损坏、或 PID 已退出，均视为不可用（返回 error 并顺带清理失效文件）。
func ReadEndpointFile() (*EndpointInfo, error) {
	b, err := os.ReadFile(EndpointFilePath())
	if err != nil {
		return nil, err
	}
	var info EndpointInfo
	if err := json.Unmarshal(b, &info); err != nil {
		_ = RemoveEndpointFile()
		return nil, fmt.Errorf("corrupt endpoint file: %w", err)
	}
	if info.Socket == "" && info.Endpoint == "" {
		_ = RemoveEndpointFile()
		return nil, fmt.Errorf("endpoint file has neither socket nor endpoint")
	}
	if !pidAlive(info.PID) {
		_ = RemoveEndpointFile()
		return nil, fmt.Errorf("stale endpoint file (pid %d not alive)", info.PID)
	}
	return &info, nil
}

// EndpointAlive 探测 App 内嵌 MCP 端点是否可用（healthz，1 秒超时）。
// PID 存活只说明进程在，端点真正可用以 HTTP 探测为准。
func EndpointAlive(ctx context.Context, info *EndpointInfo) bool {
	hctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	client, url := ClientFor(info), healthzURL(info)
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// healthzURL 返回健康检查地址；unix socket 走一个占位主机名（DialContext 会忽略它）。
func healthzURL(info *EndpointInfo) string {
	if info.Transport == TransportUnix || info.Endpoint == "" {
		return "http://unix/healthz"
	}
	return strings.TrimSuffix(info.Endpoint, "/mcp") + "/healthz"
}

// MCPURL 返回用于 MCP 客户端的端点字符串。
func MCPURL(info *EndpointInfo) string {
	if info.Transport == TransportUnix || info.Endpoint == "" {
		return "http://unix/mcp"
	}
	return info.Endpoint
}

// ClientFor 构造与该端点匹配的 HTTP 客户端：
//   - unix：DialContext 直接连 socket 文件，并带上 token 头（socket 不需要，但无害）；
//   - tcp：带上 Authorization，满足服务端鉴权。
func ClientFor(info *EndpointInfo) *http.Client {
	header := http.Header{}
	if info.Token != "" {
		header.Set("Authorization", "Bearer "+info.Token)
		header.Set(TokenHeader, info.Token)
	}

	if info.Transport == TransportUnix || info.Endpoint == "" {
		path := info.Socket
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			// 每个桥接进程只连本机一个 socket，长连接复用即可。
			MaxIdleConns: 4,
		}
		return &http.Client{Transport: &headerTransport{base: tr, header: header}, Timeout: 0}
	}

	base := &http.Transport{Proxy: nil, MaxIdleConns: 4}
	return &http.Client{Transport: &headerTransport{base: base, header: header}, Timeout: 0}
}

// headerTransport 给每个请求补上固定头（token 与客户端标识）。
type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone 避免修改调用方（SDK）复用的请求对象。
	r := req.Clone(req.Context())
	for k, vs := range t.header {
		for _, v := range vs {
			r.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(r)
}
