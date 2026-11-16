package mcpserver

// MCP logcat 日志流资源：让 MCP 客户端以"游标增量读"的方式跟踪设备日志。
//
// 为什么不复用 backend/adb.StartLogcatStream：那个函数按 deviceId 全局单例，
// 重复启动会先 kill 已有流——MCP 一读日志就会把 GUI 日志页的流顶掉。
// 所以这里为 MCP 单独起 adb 进程，两边互不影响。
//
// 设计：
//   - 每个设备一个 collector：独立 `adb -s <key> logcat -v threadtime` 进程，
//     输出进固定容量环形缓冲（2000 条），seq 单调递增；
//   - 客户端先读 tail 拿快照与 nextCursor，之后用 since/{cursor} 增量轮询；
//   - 进程退出（设备断开等）标记 dead，下次读取自动重拉，调用方无感；
//   - 读取时顺带回收：设备不在线或超过 10 分钟无人读的 collector 停止并移除，
//     不留常驻进程。
//
// 仅支持 Android（logcat）。鸿蒙走 hilog，语义差异大，暂不并入，读取时明确报错。

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"Hadice/backend/adb"
	"Hadice/backend/device"
)

const (
	logRingCap        = 2000             // 每设备环形缓冲条数
	logSinceLimit     = 500              // 单次增量最多返回条数
	logTailDefault    = 200              // tail 快照返回条数
	logIdleTTL        = 10 * time.Minute // 无人读取多久后回收进程
	logMaxCollectors  = 8                // 同时订阅的设备数上限
	logRestartBackoff = 2 * time.Second  // 进程异常退出后的重拉间隔
)

// LogLine 是日志流里的一条记录（Seq 是跨次读取单调递增的游标）。
type LogLine struct {
	Seq     int64  `json:"seq"`
	Time    string `json:"time"`
	Pid     int    `json:"pid"`
	Tid     int    `json:"tid"`
	Level   string `json:"level"`
	Tag     string `json:"tag"`
	Message string `json:"message"`
}

// LogSnapshot 是一次资源读取的返回。
type LogSnapshot struct {
	Device    string    `json:"device"`
	Entries   []LogLine `json:"entries"`
	Cursor    int64     `json:"cursor"`    // 本次读取到的最大 seq（作为下次 since 的入参）
	Truncated bool      `json:"truncated"` // 游标之后的日志因环形缓冲溢出被丢弃
	Running   bool      `json:"running"`   // 采集进程是否存活
}

// logCollector 是单个设备的日志采集器。
type logCollector struct {
	key      string
	mu       sync.Mutex
	buf      []LogLine // 定长环形，满则丢最旧
	capacity int
	nextSeq  int64
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	dead     bool      // 进程已退出，需要重拉
	lastRead time.Time // 活跃度，用于回收
}

func newLogCollector(key string) *logCollector {
	return &logCollector{key: key, capacity: logRingCap, nextSeq: 1, lastRead: time.Now()}
}

// append 追加一条日志（由读取 goroutine 调用）。
func (c *logCollector) append(line LogLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line.Seq = c.nextSeq
	c.nextSeq++
	c.buf = append(c.buf, line)
	if len(c.buf) > c.capacity {
		c.buf = c.buf[len(c.buf)-c.capacity:]
	}
}

// markDead 标记采集进程退出。
func (c *logCollector) markDead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dead = true
	c.cmd = nil
}

// alive 判断进程是否在跑。
func (c *logCollector) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.dead && c.cmd != nil
}

// tail 返回最近 n 条快照。
func (c *logCollector) tail(n int) LogSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRead = time.Now()
	start := 0
	if len(c.buf) > n && n > 0 {
		start = len(c.buf) - n
	}
	entries := make([]LogLine, len(c.buf)-start)
	copy(entries, c.buf[start:])
	return LogSnapshot{
		Device:  c.key,
		Entries: entries,
		Cursor:  c.nextSeq - 1,
		Running: !c.dead && c.cmd != nil,
	}
}

// since 返回 cursor 之后的增量（含溢出标记）。
func (c *logCollector) since(cursor int64) LogSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRead = time.Now()

	entries := make([]LogLine, 0, logSinceLimit)
	oldest := int64(0)
	if len(c.buf) > 0 {
		oldest = c.buf[0].Seq
	}
	truncated := oldest > 0 && cursor > 0 && oldest > cursor+1
	for i := range c.buf {
		if c.buf[i].Seq <= cursor {
			continue
		}
		entries = append(entries, c.buf[i])
		if len(entries) >= logSinceLimit {
			break
		}
	}
	return LogSnapshot{
		Device:    c.key,
		Entries:   entries,
		Cursor:    c.nextSeq - 1,
		Truncated: truncated,
		Running:   !c.dead && c.cmd != nil,
	}
}

// stop 杀掉采集进程。
func (c *logCollector) stop() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ensure 起采集进程（若未运行）。返回进程是否本次新拉起。
func (c *logCollector) ensure() (bool, error) {
	c.mu.Lock()
	if !c.dead && c.cmd != nil {
		c.mu.Unlock()
		return false, nil
	}
	if err := adb.EnsureAdbExecutable(); err != nil {
		c.mu.Unlock()
		return false, fmt.Errorf("adb 不可用: %w", err)
	}
	adbPath := adb.GetAdbPath()
	if adbPath == "" {
		c.mu.Unlock()
		return false, fmt.Errorf("adb 路径未就绪")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, adbPath, "-s", c.key, "logcat", "-v", "threadtime")
	adb.HideWindowsConsoleWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		c.mu.Unlock()
		return false, fmt.Errorf("创建日志管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		c.mu.Unlock()
		return false, fmt.Errorf("启动 logcat 失败（设备可能已断开）: %w", err)
	}
	c.cmd = cmd
	c.cancel = cancel
	c.dead = false
	c.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			c.append(parseLogcatLine(line))
		}
		_ = cmd.Wait()
		c.markDead()
	}()
	return true, nil
}

// parseLogcatLine 解析 threadtime 格式一行。
// 形如 `11-13 00:27:33.123  1234  1235 D Tag    : message`；
// 分隔行（--------- beginning of ...）按原文保留。
func parseLogcatLine(line string) LogLine {
	entry := LogLine{Message: line}
	parts := strings.Fields(line)
	if len(parts) >= 6 {
		entry.Time = parts[0] + " " + parts[1]
		entry.Pid, _ = strconv.Atoi(parts[2])
		entry.Tid, _ = strconv.Atoi(parts[3])
		entry.Level = parts[4]
		rest := strings.Join(parts[5:], " ")
		if idx := strings.Index(rest, ":"); idx >= 0 {
			entry.Tag = strings.TrimSpace(rest[:idx])
			entry.Message = strings.TrimSpace(rest[idx+1:])
		} else {
			entry.Message = rest
		}
	}
	return entry
}

// logHub 管理所有设备的 collector。
type logHub struct {
	rt         *Runtime
	mu         sync.Mutex
	collectors map[string]*logCollector
}

func newLogHub(rt *Runtime) *logHub {
	return &logHub{rt: rt, collectors: make(map[string]*logCollector)}
}

// read 统一入口：保证采集进程在跑、做回收，再返回快照。
func (h *logHub) read(key string, cursor int64, tailN int) (*LogSnapshot, error) {
	platform := h.rt.PlatformFor(key)
	if platform == "" {
		return nil, fmt.Errorf("设备 %s 当前不在线（或未识别），无法读取日志；可先调用 list_devices 确认", key)
	}
	if platform != device.PlatformAndroid {
		return nil, fmt.Errorf("日志流资源（resources/read）目前只镜像 Android logcat 缓冲区；"+
			"鸿蒙设备请调用 device_logcat 工具（自动走 hilog）：%s", key)
	}

	h.mu.Lock()
	c, ok := h.collectors[key]
	if !ok {
		h.pruneLocked()
		if len(h.collectors) >= logMaxCollectors {
			h.mu.Unlock()
			return nil, fmt.Errorf("同时订阅的设备数已达上限（%d），稍后再试", logMaxCollectors)
		}
		c = newLogCollector(key)
		h.collectors[key] = c
	}
	h.mu.Unlock()

	if _, err := c.ensure(); err != nil {
		return nil, err
	}
	snap := c.snapshot(cursor, tailN)
	return &snap, nil
}

// snapshot 按游标语义分发到 tail / since。
func (c *logCollector) snapshot(cursor int64, tailN int) LogSnapshot {
	if cursor > 0 {
		return c.since(cursor)
	}
	if tailN <= 0 {
		tailN = logTailDefault
	}
	return c.tail(tailN)
}

// pruneLocked 回收不活跃/已离线的 collector（调用方持 h.mu）。
func (h *logHub) pruneLocked() {
	now := time.Now()
	for key, c := range h.collectors {
		c.mu.Lock()
		idle := now.Sub(c.lastRead)
		c.mu.Unlock()
		// 离线设备或超时未读：停进程并移除。PlatformFor 缓存未命中才刷设备列表，
		// 正常轮询场景开销为零。
		if idle > logIdleTTL || (idle > logRestartBackoff && h.rt.PlatformFor(key) == "") {
			c.stop()
			delete(h.collectors, key)
		}
	}
}

// stopAll 停止全部采集进程（App 停止 MCP / 退出时调用）。
func (h *logHub) stopAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.collectors {
		c.stop()
	}
	h.collectors = make(map[string]*logCollector)
}

// 日志资源 URI 解析：
//
//	hadice://device/{key}/logcat/tail        最近 200 条快照
//	hadice://device/{key}/logcat/since/{seq} seq 之后的增量
var (
	logTailURIRe  = regexp.MustCompile(`^hadice://device/([^/]+)/logcat/tail$`)
	logSinceURIRe = regexp.MustCompile(`^hadice://device/([^/]+)/logcat/since/(\d+)$`)
)

// parseLogURI 解析日志资源 URI，返回 connectKey 与游标（tail 为 0）。
func parseLogURI(uri string) (key string, cursor int64, err error) {
	if m := logTailURIRe.FindStringSubmatch(uri); m != nil {
		return unescapeURIKey(m[1])
	}
	if m := logSinceURIRe.FindStringSubmatch(uri); m != nil {
		key, err := url.PathUnescape(m[1])
		if err != nil {
			return "", 0, fmt.Errorf("URI 中的设备标识无效: %w", err)
		}
		cursor, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("URI 中的游标无效: %w", err)
		}
		return key, cursor, nil
	}
	return "", 0, fmt.Errorf("无法识别的日志资源 URI: %s", uri)
}

func unescapeURIKey(raw string) (string, int64, error) {
	key, err := url.PathUnescape(raw)
	if err != nil {
		return "", 0, fmt.Errorf("URI 中的设备标识无效: %w", err)
	}
	return key, 0, nil
}
