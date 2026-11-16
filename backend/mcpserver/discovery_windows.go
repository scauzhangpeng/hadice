//go:build windows

package mcpserver

// pidAlive 在 Windows 上没有 signal(0) 等价物，直接返回 true，
// 存活判断完全依赖 EndpointAlive 的 healthz HTTP 探测。
func pidAlive(pid int) bool {
	return pid > 0
}
