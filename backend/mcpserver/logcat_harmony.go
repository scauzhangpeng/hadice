package mcpserver

// 鸿蒙侧日志读取（hilog）。
//
// 与 Android 的 logcat 对应关系：
//
//	logcat -d          →  hilog -x        （导出整个缓冲区后退出）
//	timeout N logcat   →  timeout N hilog （设备侧 timeout 杀进程收尾）
//	logcat --pid=P     →  hilog -P P
//	logcat TAG:LEVEL   →  hilog -T TAG -L LEVEL
//
// 与 logcat 的一处实质差异：hilog 默认会**先回放整个缓冲区再持续输出**，
// 单靠 `timeout N hilog` 拿到的 N 秒窗口里混着几小时前的历史日志（实测
// 2 秒窗口带回 4466 行、时间戳早于命令本身）。因此采样模式必须自己按
// 时间戳截断：先取设备当前时间，只保留 >= 该时刻的行。
//
// 两个平台返回同一套 logcatResult 结构，模型不需要区分数据来源。
// 级别差异：Android 有 V(verbose)，鸿蒙最低是 D(DEBUG)，level=V 时不加
// -L 参数（等价"全部级别"）。

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"Hadice/backend/device"
	"Hadice/backend/hdc"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hilogTimeLayout 是 hilog 行首的时间格式（"11-13 17:41:20.196"），
// 只取到秒（前 14 字符）用于窗口截断，毫秒级差异对采样含义无影响。
const hilogTimeLayout = "01-02 15:04:05"

func (r *Runtime) harmonyLogcat(connectKey string, in logcatIn, level string, maxLines, duration int, re *regexp.Regexp) (*mcp.CallToolResult, struct{}, error) {
	out := logcatResult{Level: level, DurationMs: duration, Lines: []string{}}

	args := []string{"-t", connectKey, "shell"}
	since := ""
	if duration > 0 {
		secs := (duration + 999) / 1000
		// 先取设备时间：采样窗口必须按设备时钟切，本机时钟可能有偏差。
		since = hdcDeviceTime(connectKey)
		args = append(args, "timeout", strconv.Itoa(secs), "hilog")
	} else {
		// -x 导出整个缓冲区（真机约 8900 行 / 1.6MB），再由本地截取尾部 N 条。
		// 不用 -z：它是"缓冲区尾部 N 条原始行"，过滤级别/进程后条数会莫名变少。
		args = append(args, "hilog", "-x")
	}

	if app := strings.TrimSpace(in.App); app != "" {
		pkg, err := r.resolvePackage(device.PlatformHarmonyOS, connectKey, app)
		if err != nil {
			return fail("%v", err)
		}
		pid := hdc.PidOf(connectKey, pkg)
		if pid <= 0 {
			return fail("应用 %s 当前未运行，无法按进程过滤日志；请先调用 launch_app 启动它", pkg)
		}
		out.PackageName, out.Pid = pkg, pid
		args = append(args, "-P", strconv.Itoa(pid))
	}
	if tag := strings.TrimSpace(in.Tag); tag != "" {
		args = append(args, "-T", tag)
	}
	// V 在鸿蒙没有对应级别，视为"不过滤"。
	if level != "V" {
		args = append(args, "-L", level)
	}

	execTimeout := 20 * time.Second
	if duration > 0 {
		execTimeout = time.Duration(duration)*time.Millisecond + 15*time.Second
	}
	res, err := hdc.ExecuteHdcWithTimeout(args, execTimeout)
	if res == nil {
		return fail("读取 hilog 失败: 无返回")
	}
	// timeout 到点会以非零码退出，这是预期收尾方式，只要拿到输出就继续。
	if err != nil && strings.TrimSpace(res.Output) == "" {
		return fail("读取 hilog 失败: %s", hdcErrText(res, err))
	}

	raw := 0
	dropped := 0
	for _, line := range strings.Split(res.Output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "[HDC]") {
			continue
		}
		raw++
		// 采样模式下丢弃窗口开始前的历史回放。
		if since != "" {
			if ts := hilogLineTime(line); ts != "" && ts < since {
				dropped++
				continue
			}
		}
		if re != nil && !re.MatchString(line) {
			continue
		}
		out.Lines = append(out.Lines, line)
	}
	out.RawLineCount = raw - dropped
	if len(out.Lines) > maxLines {
		out.Lines = out.Lines[len(out.Lines)-maxLines:]
		out.Truncated = true
	}
	out.LineCount = len(out.Lines)
	if out.Truncated {
		out.Hint = fmt.Sprintf("窗口内共 %d 行，仅返回最后 %d 行；可提高 level（如 I/W/E）、"+
			"用 tag/filter 缩小范围，或调低 durationMs", out.RawLineCount, out.LineCount)
	} else if duration == 0 && out.LineCount >= maxLines {
		// 缓冲模式由 `hilog -z N` 自身限定条数，拿不到缓冲区总行数，
		// 不能说"被截断"（否则模型会以为漏了日志而反复调大 maxLines）。
		out.Hint = fmt.Sprintf("已返回缓冲区最后 %d 行（受 maxLines 限制）；需要更早的日志请调大 maxLines，"+
			"想观察「现在发生了什么」请改用 durationMs 实时采样", out.LineCount)
	}
	if out.LineCount == 0 {
		return okJSON(map[string]any{
			"packageName":  out.PackageName,
			"pid":          out.Pid,
			"level":        out.Level,
			"durationMs":   out.DurationMs,
			"rawLineCount": out.RawLineCount,
			"lineCount":    0,
			"lines":        []string{},
			"hint":         "没有匹配的日志。可尝试降低 level（如 D）、去掉 filter/tag，或操作应用后再读取。",
		})
	}
	return okJSON(out)
}

// hdcDeviceTime 返回设备当前时间（"01-02 15:04:05"，与 hilog 行首格式一致）。
// 取不到时返回空串——此时退化为"不截断历史"，好过把整个工具调用弄失败。
func hdcDeviceTime(connectKey string) string {
	res, err := hdc.ExecuteHdcWithTimeout(
		[]string{"-t", connectKey, "shell", "date", "+" + hilogTimeLayout}, 10*time.Second)
	if err != nil || res == nil || !res.Success {
		return ""
	}
	ts := strings.TrimSpace(res.Output)
	if len(ts) != len("01-02 15:04:05") {
		return ""
	}
	return ts
}

// hilogLineTime 取 hilog 行首的 "MM-DD HH:MM:SS"；格式不符返回空串。
func hilogLineTime(line string) string {
	const n = len("01-02 15:04:05")
	if len(line) < n {
		return ""
	}
	ts := line[:n]
	// 简单形状校验，避免把非日志行误当时间比较。
	if ts[2] != '-' || ts[5] != ' ' || ts[8] != ':' || ts[11] != ':' {
		return ""
	}
	return ts
}
