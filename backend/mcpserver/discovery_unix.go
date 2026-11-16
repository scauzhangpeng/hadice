//go:build !windows

package mcpserver

import "syscall"

// pidAlive 用 signal 0 探测进程存活（仅 Unix；Windows 侧见 discovery_windows.go）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
