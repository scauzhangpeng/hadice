package mcpserver

// TCP 端点鉴权。
//
// 威胁模型（本机单用户桌面应用）：
//   - 同用户的其它进程本来就能直接跑 adb / 读写配置，不在防御范围内
//     （真正要防这类攻击需要系统级沙箱，不是 token 能解决的）；
//   - 真正现实的风险来自**浏览器**：恶意网页可以 fetch("http://127.0.0.1:端口")
//     或利用 DNS rebinding 把域名解析到 127.0.0.1，从而在没有用户感知的情况下
//     调用工具（例如 uninstall_app）。
//
// 因此这里做三件事，缺一不可：
//  1. Host 白名单 —— 只接受 127.0.0.1 / localhost / [::1]，挡掉 DNS rebinding；
//  2. Origin 白名单 —— 带 Origin 头且非回环来源的请求一律拒绝，挡掉跨站请求；
//  3. Bearer token —— 随机 token 只写在 0600 的 discovery 文件里，网页读不到，
//     因此无法伪造；桥接进程读文件后自动带上。
//
// Unix domain socket 不走这里：socket 文件权限 0600 本身就是鉴权（同用户可读，
// 其它用户不可读），且无法被浏览器访问。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"strings"
)

// TokenHeader 是桥接进程携带 token 的自定义头（除标准的 Authorization 之外的便捷写法）。
const TokenHeader = "X-Hadice-Token"

// ClientHeader 由桥接进程写入，标识"谁在调用"（父进程名，如 WorkBuddy）。
// 仅用于审计展示，不参与鉴权。
const ClientHeader = "X-Hadice-Client"

// NewToken 生成 32 字节随机 token（hex 编码）。
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源异常，此时宁可拒绝服务也不要放行。
		log.Printf("[%s] generate token failed: %v", ServerName, err)
		return ""
	}
	return hex.EncodeToString(b)
}

// TCPAuth 返回 TCP 监听用的中间件。
// token 为空时只做 Host/Origin 校验（测试或用户显式关闭鉴权）。
func TCPAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackHost(r.Host) {
				http.Error(w, "forbidden: non-loopback Host", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
			// 健康检查不返回任何能力信息，允许无 token 探测（桥接进程启动时判断
			// App 是否就绪会先打这个口子）。
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			if token != "" && !validToken(r, token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hadice-mcp"`)
				http.Error(w, "unauthorized: missing or invalid token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func validToken(r *http.Request, token string) bool {
	got := r.Header.Get(TokenHeader)
	if got == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			got = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if got == "" {
		// 兜底：部分只支持 URL 的客户端（如某些 IDE 插件）只能用 query 传。
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// isLoopbackHost 校验 Host 头（含端口）是否为回环地址。
func isLoopbackHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackOrigin 校验 Origin 头（形如 http://127.0.0.1:5173）。
func isLoopbackOrigin(origin string) bool {
	// "null" 来自 file:// 或沙箱 iframe，一律拒绝（无法区分来源）。
	if origin == "" || strings.EqualFold(origin, "null") {
		return false
	}
	rest := origin
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return isLoopbackHost(rest)
}
