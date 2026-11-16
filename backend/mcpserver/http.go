package mcpserver

// 承载层：把同一个 MCP handler 暴露到两种本地传输上。
//
//	Unix domain socket  —— 默认给 hadice-mcp 桥接用：文件权限 0600 即鉴权，
//	                       浏览器无法访问，也没有端口冲突与端口扫描面。
//	TCP 127.0.0.1       —— 给只能填 URL 的客户端用：随机端口 + token +
//	                       Host/Origin 白名单（见 auth.go）。
//
// 两种传输共享同一个 *mcp.Server 实例，会话互不干扰。

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeOptions 是监听参数。
type ServeOptions struct {
	// TCPAddr TCP 监听地址，如 "127.0.0.1:0"（0 表示随机端口）；空表示不监听 TCP。
	TCPAddr string
	// SocketPath Unix socket 路径；空表示不监听 socket。
	SocketPath string
	// Token TCP 端点所需的 Bearer token；空表示不校验 token。
	Token string
	// DisableAuthHostCheck 仅供测试使用：跳过 Host/Origin 校验。
	DisableAuthHostCheck bool
}

// Endpoints 是监听成功后的实际端点信息（写入 discovery 供桥接发现）。
type Endpoints struct {
	// TCP 形如 http://127.0.0.1:52341/mcp；未监听时为空。
	TCP string `json:"tcp,omitempty"`
	// Socket Unix socket 文件路径；未监听时为空。
	Socket string `json:"socket,omitempty"`
	// Token TCP 端点鉴权用的 token。
	Token string `json:"token,omitempty"`
}

// DefaultSocketPath 返回默认 Unix socket 路径（用户配置目录下，长度远小于 sun_path 上限）。
func DefaultSocketPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "hadice-mcp.sock")
	}
	return filepath.Join(dir, "hadice", "mcp.sock")
}

// Serve 启动监听，返回实际端点与停服函数。
func Serve(server *mcp.Server, opt ServeOptions) (Endpoints, func(), error) {
	var eps Endpoints
	eps.Token = opt.Token

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	var servers []*http.Server
	shutdown := func() {
		for _, s := range servers {
			_ = s.Close()
		}
	}

	if opt.SocketPath != "" {
		// 残留 socket 文件（上次异常退出）必须清掉，否则 Listen 报 address already in use。
		_ = os.Remove(opt.SocketPath)
		if err := os.MkdirAll(filepath.Dir(opt.SocketPath), 0o755); err != nil {
			return eps, func() {}, fmt.Errorf("prepare socket dir: %w", err)
		}
		ln, err := net.Listen("unix", opt.SocketPath)
		if err != nil {
			return eps, func() {}, fmt.Errorf("listen unix socket: %w", err)
		}
		// 文件权限就是鉴权：只有当前用户可读写。
		if err := os.Chmod(opt.SocketPath, 0o600); err != nil {
			_ = ln.Close()
			return eps, func() {}, fmt.Errorf("chmod socket: %w", err)
		}
		hs := &http.Server{Handler: mux}
		servers = append(servers, hs)
		go func() {
			if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("[%s] unix socket server error: %v", ServerName, err)
			}
		}()
		eps.Socket = opt.SocketPath
		log.Printf("[%s] unix socket listening on %s (mode 0600)", ServerName, opt.SocketPath)
	}

	if opt.TCPAddr != "" {
		ln, err := net.Listen("tcp", opt.TCPAddr)
		if err != nil {
			shutdown()
			return eps, func() {}, fmt.Errorf("listen tcp: %w", err)
		}
		var tcpHandler http.Handler = mux
		if !opt.DisableAuthHostCheck {
			tcpHandler = TCPAuth(opt.Token)(mux)
		}
		hs := &http.Server{Handler: tcpHandler}
		servers = append(servers, hs)
		go func() {
			if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("[%s] http server error: %v", ServerName, err)
			}
		}()
		host, port, _ := net.SplitHostPort(ln.Addr().String())
		eps.TCP = "http://" + net.JoinHostPort(host, port) + "/mcp"
		authNote := "token auth"
		if opt.Token == "" {
			authNote = "NO token"
		}
		log.Printf("[%s] streamable-http listening on %s (%s, loopback only)", ServerName, eps.TCP, authNote)
	}

	if eps.TCP == "" && eps.Socket == "" {
		return eps, func() {}, fmt.Errorf("no transport enabled")
	}

	return eps, func() {
		shutdown()
		if opt.SocketPath != "" {
			_ = os.Remove(opt.SocketPath)
		}
	}, nil
}

// ServeHTTP 是 Serve 的简化包装（仅 TCP），保留给测试与旧调用方使用。
func ServeHTTP(server *mcp.Server, addr string, onListen func(net.Addr)) (func(), error) {
	eps, closeFn, err := Serve(server, ServeOptions{TCPAddr: addr})
	if err != nil {
		return nil, err
	}
	if onListen != nil {
		// 复用原有回调语义：从端点字符串还原地址。
		hostport := ""
		if eps.TCP != "" {
			hostport = eps.TCP[len("http://") : len(eps.TCP)-len("/mcp")]
		}
		if a, aerr := net.ResolveTCPAddr("tcp", hostport); aerr == nil {
			onListen(a)
		}
	}
	return closeFn, nil
}
