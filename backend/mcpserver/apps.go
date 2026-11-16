package mcpserver

// 应用域工具：启动应用、屏幕截图、日志读取。
//
// 典型编排（由 MCP 客户端/模型组合调用）：
//
//	launch_app → device_screenshot        验证应用是否正确启动（看到界面）
//	launch_app → device_logcat            查看启动后的进程日志（按 pid 过滤）
//
// 两个平台复用各自 GUI 侧已在用的实现（adb: StartApp / TakeScreenshot /
// GetAppList；hdc: StartApp / TakeScreenshot / GetAppList），不重复造命令
// 拼接逻辑；平台差异（Activity vs Ability、logcat vs hilog）在 handler 内
// 按 list_devices 给出的 platform 分派，工具名与返回结构保持一致。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"Hadice/backend/adb"
	"Hadice/backend/device"
	"Hadice/backend/hdc"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerAppTools 注册应用域工具。
func (r *Runtime) registerAppTools(s *mcp.Server) {
	registerTool(r, s, &mcp.Tool{
		Name: "list_apps",
		Description: "列出设备上已安装的应用（包名、是否运行、是否系统应用），可用 filter " +
			"按包名子串筛选。用于在只知道应用中文名/模糊名称时定位包名，结果可传给 " +
			"launch_app / device_logcat。",
		Annotations: &mcp.ToolAnnotations{
			Title:         "列出已安装应用",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, r.listApps)

	registerTool(r, s, &mcp.Tool{
		Name: "launch_app",
		Description: "在设备上启动指定应用，并返回启动结果：进程 pid、当前前台 Activity、" +
			"是否已在前台。app 传包名（如 com.tencent.mm）或包名片段（如 tencent.mm）；" +
			"只有中文应用名时先用 list_apps 查包名。restart=true 会先 force-stop，" +
			"用于观察冷启动；常与 device_screenshot（看界面）、device_logcat（看日志）搭配。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "启动应用",
			ReadOnlyHint:    false,
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(false),
		},
	}, r.launchApp)

	registerTool(r, s, &mcp.Tool{
		Name: "device_screenshot",
		Description: "截取设备当前屏幕并直接返回图片（Android 为 PNG，鸿蒙为 JPEG），" +
			"同时给出本地保存路径与分辨率。用于确认界面状态——例如启动应用后检查是否停在预期页面；" +
			"配合 device_tap 可形成\"截图 → 看图定位 → 点击 → 再截图复验\"的闭环。",
		Annotations: &mcp.ToolAnnotations{
			Title:         "截取设备屏幕",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, r.deviceScreenshot)

	registerTool(r, s, &mcp.Tool{
		Name: "device_logcat",
		Description: "读取设备日志（Android 走 logcat，鸿蒙走 hilog），两个平台返回同一套结构。" +
			"指定 app 时只返回该应用进程（pid）的日志，用于定位应用启动/运行问题；" +
			"可用 level 限制最低级别、filter 做正则过滤、maxLines 限制条数。" +
			"传 durationMs 则改为实时采样该时长（如 10000 采集 10 秒），" +
			"适合观察「打开应用后这 10 秒发生了什么」。应用未运行时提示先调用 launch_app。",
		Annotations: &mcp.ToolAnnotations{
			Title:         "读取应用日志（logcat）",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, r.deviceLogcat)
}

// ---------- list_apps ----------

type listAppsIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	// Filter 按包名子串过滤（大小写不敏感），空则返回全部。
	Filter string `json:"filter,omitempty" jsonschema:"包名子串过滤，如 tencent；为空返回全部应用"`
	// Limit 限制返回条数，默认 100，最大 500。
	Limit int `json:"limit,omitempty" jsonschema:"最多返回条数，默认 100，最大 500"`
}

func (r *Runtime) listApps(_ context.Context, _ *mcp.CallToolRequest, in listAppsIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	lower := strings.ToLower(strings.TrimSpace(in.Filter))
	type appItem struct {
		PackageName string `json:"packageName"`
		IsRunning   bool   `json:"isRunning"`
		IsSystemApp bool   `json:"isSystemApp"`
	}
	out := make([]appItem, 0, limit)
	matched := 0
	add := func(name string, isRunning, isSystem bool) {
		matched++
		if len(out) >= limit {
			return
		}
		out = append(out, appItem{name, isRunning, isSystem})
	}

	if p == device.PlatformHarmonyOS {
		// 只拉清单，运行态另走 ps -ef（见 hdc.RunningPackages 的说明）：
		// GetAppList 内部会并行跑 `ps -A`，而鸿蒙的 ps -A 看不到应用进程，
		// 那一次调用纯属浪费，这里用 ListBundles 省掉。
		apps, err := hdc.ListBundles(in.ConnectKey)
		if err != nil {
			return fail("获取应用列表失败: %v", err)
		}
		running := hdc.RunningPackages(in.ConnectKey)
		for _, a := range apps {
			if lower != "" && !strings.Contains(strings.ToLower(a.PackageName), lower) {
				continue
			}
			add(a.PackageName, running[a.PackageName], a.IsSystemApp)
		}
	} else {
		list, err := adb.GetAppList(in.ConnectKey, "all")
		if err != nil {
			return fail("获取应用列表失败: %v", err)
		}
		for _, a := range list.Apps {
			if lower != "" && !strings.Contains(strings.ToLower(a.PackageName), lower) {
				continue
			}
			add(a.PackageName, a.IsRunning, a.IsSystemApp)
		}
	}
	// total 是"匹配到的总数"，returned 是"本次返回的条数"。
	// 两者分开后，调用方才能判断该不该加 limit 继续翻——
	// 之前 total 直接等于返回条数，恰好取满时无法区分"只有这么多"还是"被截断"。
	return okJSON(map[string]any{
		"total":     matched,
		"returned":  len(out),
		"truncated": matched > len(out),
		"apps":      out,
	})
}

// ---------- launch_app ----------

type launchAppIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	App        string `json:"app" jsonschema:"应用包名（如 com.tencent.mm）或包名片段（如 tencent.mm）"`
	// WaitMs 启动后等待时长，默认 1500ms（给首帧渲染留时间）。
	WaitMs int `json:"waitMs,omitempty" jsonschema:"启动后等待毫秒数，默认 1500，最大 10000"`
	// Restart 为 true 时先 force-stop 再启动，用于观察冷启动过程。
	Restart bool `json:"restart,omitempty" jsonschema:"是否先强杀进程再启动（冷启动），默认 false；配合 device_logcat 可采集启动阶段日志"`
}

type launchResult struct {
	PackageName        string `json:"packageName"`
	Launched           bool   `json:"launched"`
	Pid                int    `json:"pid"`
	ForegroundActivity string `json:"foregroundActivity"`
	IsForeground       bool   `json:"isForeground"`
	Hint               string `json:"hint,omitempty"`
}

func (r *Runtime) launchApp(_ context.Context, _ *mcp.CallToolRequest, in launchAppIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	pkg, err := r.resolvePackage(p, in.ConnectKey, in.App)
	if err != nil {
		return fail("%v", err)
	}

	wait := in.WaitMs
	if wait <= 0 {
		wait = 1500
	}
	if wait > 10000 {
		wait = 10000
	}

	out := launchResult{PackageName: pkg}

	if p == device.PlatformHarmonyOS {
		if in.Restart {
			// 冷启动：先 force-stop，保证拿到的是完整启动链路。
			_, _ = hdc.StopApp(in.ConnectKey, pkg)
			time.Sleep(300 * time.Millisecond)
		}
		res, serr := hdc.StartApp(in.ConnectKey, pkg)
		if serr != nil || res == nil || !res.Success {
			return fail("启动 %s 失败: %s", pkg, hdcErrText(res, serr))
		}
		time.Sleep(time.Duration(wait) * time.Millisecond)
		out.Pid = hdc.PidOf(in.ConnectKey, pkg)
		out.Launched = out.Pid > 0
		// 鸿蒙没有 Activity 概念，前台判断落到 bundleName 上；
		// ForegroundApp 返回 "包名/.Ability"，因此按包名做前缀比较。
		out.ForegroundActivity = hdc.ForegroundApp(in.ConnectKey)
		out.IsForeground = out.ForegroundActivity == pkg ||
			strings.HasPrefix(out.ForegroundActivity, pkg+"/")
		switch {
		case !out.Launched:
			out.Hint = "aa start 返回成功但未找到进程：可能 ability 名不匹配或应用启动即崩溃，建议截图确认"
		case !out.IsForeground && out.ForegroundActivity != "":
			out.Hint = "应用进程在运行，但当前前台是 " + out.ForegroundActivity +
				"：可能存在启动页跳转、权限弹窗或崩溃回到桌面，建议截图确认"
		}
		return okJSON(out)
	}

	if in.Restart {
		// 冷启动：先杀掉旧进程，保证拿到的是完整的启动链路日志。
		_, _ = adb.ExecuteAdbWithTimeout(
			[]string{"-s", in.ConnectKey, "shell", "am", "force-stop", pkg}, 10*time.Second)
		time.Sleep(300 * time.Millisecond)
	}

	res, err := adb.StartApp(in.ConnectKey, pkg)
	if err != nil {
		return fail("启动 %s 失败: %v", pkg, err)
	}
	if res != nil && !res.Success {
		return fail("启动 %s 失败: %s", pkg, strings.TrimSpace(res.Output))
	}

	time.Sleep(time.Duration(wait) * time.Millisecond)

	out.Pid = pidOf(in.ConnectKey, pkg)
	out.Launched = out.Pid > 0
	out.ForegroundActivity = foregroundActivity(in.ConnectKey)
	out.IsForeground = out.ForegroundActivity != "" && strings.Contains(out.ForegroundActivity, pkg)

	switch {
	case !out.Launched:
		out.Hint = "未找到应用进程，可能启动失败（应用被禁用/崩溃）或包名不正确"
	case !out.IsForeground:
		out.Hint = "应用进程在运行，但当前前台不是该应用：可能存在启动页跳转、权限弹窗或崩溃回到桌面，建议截图确认"
	}
	return okJSON(out)
}

// ---------- device_screenshot ----------

type screenshotIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	// SaveDir 本地保存目录，空则用默认的 ~/Documents/Hadice/screenshot。
	SaveDir string `json:"saveDir,omitempty" jsonschema:"本地保存目录，留空使用默认目录"`
}

func (r *Runtime) deviceScreenshot(_ context.Context, _ *mcp.CallToolRequest, in screenshotIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}

	if p == device.PlatformHarmonyOS {
		// 鸿蒙侧用 snapshot_display 出 jpeg：真机实测 318KB，而 uitest screenCap
		// 的 png 是 3.5MB——同样能读清界面文字，jpeg 对上下文与传输都更友好。
		info, err := hdc.TakeScreenshot(in.ConnectKey, in.SaveDir, "jpeg")
		if err != nil {
			return fail("截图失败: %v", err)
		}
		data, err := os.ReadFile(info.LocalPath)
		if err != nil {
			return fail("读取截图文件失败: %v", err)
		}
		if len(data) == 0 {
			return fail("截图文件为空: %s", info.LocalPath)
		}
		return screenshotContent(data, info.LocalPath, info.FileName, info.Width, info.Height, info.Size)
	}

	info, err := adb.TakeScreenshot(in.ConnectKey, in.SaveDir, "png")
	if err != nil {
		return fail("截图失败: %v", err)
	}
	data, err := os.ReadFile(info.LocalPath)
	if err != nil {
		return fail("读取截图文件失败: %v", err)
	}
	if len(data) == 0 {
		return fail("截图文件为空: %s", info.LocalPath)
	}
	// 宽高以 PNG IHDR 头为准：adb.TakeScreenshot 用 wm size 推断，在设置了
	// Override size 的设备上会与实际图像不符（真机实测 1344x2772 vs 1152x2376）。
	w, h := pngSize(data)
	if w > 0 {
		info.Width, info.Height = w, h
	}
	return screenshotContent(data, info.LocalPath, info.FileName, info.Width, info.Height, info.Size)
}

// screenshotContent 组装"图片 + 元信息"的工具返回（两个平台共用）。
func screenshotContent(data []byte, localPath, fileName string, width, height int, size int64) (*mcp.CallToolResult, struct{}, error) {
	meta, merr := json.MarshalIndent(map[string]any{
		"localPath": localPath,
		"fileName":  fileName,
		"width":     width,
		"height":    height,
		"size":      size,
	}, "", "  ")
	if merr != nil {
		return fail("序列化截图信息失败: %v", merr)
	}
	// 图片内容 + 元信息一起返回：模型既能"看"界面，也能拿到本地路径。
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: data, MIMEType: imageMIME(localPath)},
			&mcp.TextContent{Text: string(meta)},
		},
	}, struct{}{}, nil
}

// imageMIME 按扩展名给出 MIME：Android 侧出 PNG，鸿蒙侧出 JPEG。
func imageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	default:
		return "application/octet-stream"
	}
}

// ---------- device_logcat ----------

type logcatIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	// App 为空时读取全设备日志（可能很杂，建议指定）。
	App string `json:"app,omitempty" jsonschema:"应用包名或包名片段；指定后只返回该进程的日志"`
	// Level 最低日志级别：V/D/I/W/E/F，默认 V。
	Level string `json:"level,omitempty" jsonschema:"最低日志级别 V/D/I/W/E/F，默认 V；鸿蒙 hilog 最低为 D（传 V 表示不过滤）"`
	// Tag 只保留指定 tag（需与 level 配合，其他 tag 静默）。
	Tag string `json:"tag,omitempty" jsonschema:"只保留指定 tag 的日志，如 ActivityManager"`
	// Filter 对整行做正则过滤（大小写敏感，遵循 Go regexp 语法）。
	Filter string `json:"filter,omitempty" jsonschema:"对日志行做正则过滤，如 \"Exception|crash\""`
	// MaxLines 返回条数上限，默认 200，最大 2000。
	MaxLines int `json:"maxLines,omitempty" jsonschema:"最多返回条数，默认 200，最大 2000"`
	// DurationMs 实时采样窗口：>0 时一边运行一边采集该时长内的日志（如 10000 = 10 秒），
	// 结束后返回窗口内最后 maxLines 条；0（默认）表示直接导出当前缓冲区。
	DurationMs int `json:"durationMs,omitempty" jsonschema:"实时采样窗口毫秒数，如 10000 采集 10 秒；默认 0 表示直接导出当前缓冲区"`
}

type logcatResult struct {
	PackageName  string   `json:"packageName,omitempty"`
	Pid          int      `json:"pid,omitempty"`
	Level        string   `json:"level"`
	DurationMs   int      `json:"durationMs,omitempty"`
	RawLineCount int      `json:"rawLineCount"`
	LineCount    int      `json:"lineCount"`
	Truncated    bool     `json:"truncated"`
	Hint         string   `json:"hint,omitempty"`
	Lines        []string `json:"lines"`
}

func (r *Runtime) deviceLogcat(_ context.Context, _ *mcp.CallToolRequest, in logcatIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	level := strings.ToUpper(strings.TrimSpace(in.Level))
	if level == "" {
		level = "V"
	}
	if !strings.Contains("VDIWEF", level) || len(level) != 1 {
		return fail("level 取值应为 V/D/I/W/E/F 之一，收到: %q", in.Level)
	}
	maxLines := in.MaxLines
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 2000 {
		maxLines = 2000
	}

	var re *regexp.Regexp
	if f := strings.TrimSpace(in.Filter); f != "" {
		compiled, err := regexp.Compile(f)
		if err != nil {
			return fail("filter 正则无效: %v", err)
		}
		re = compiled
	}

	// 采样窗口：下限 500ms（再短没有观测意义），上限 60s（避免长时间占用 adb 通道）。
	duration := in.DurationMs
	if duration < 0 {
		duration = 0
	}
	if duration > 0 && duration < 500 {
		duration = 500
	}
	if duration > 60000 {
		duration = 60000
	}

	if p == device.PlatformHarmonyOS {
		return r.harmonyLogcat(in.ConnectKey, in, level, maxLines, duration, re)
	}

	out := logcatResult{Level: level, DurationMs: duration, Lines: []string{}}

	// 两种读取方式：
	//   duration>0：设备侧 `timeout N logcat -T <now>` 流式采样，到时自行收尾；
	//               -T 让 logcat 只从当前时刻开始输出，窗口才是真正的"这 N 秒"，
	//               否则会把环形缓冲区里的历史日志一并带出来。
	//   duration=0：`logcat -d -t N` 直接导出缓冲区尾部 N 行。
	args := []string{"-s", in.ConnectKey}
	if duration > 0 {
		secs := (duration + 999) / 1000
		args = append(args, "shell", "timeout", strconv.Itoa(secs), "logcat", "-v", "threadtime")
		if since := deviceTime(in.ConnectKey); since != "" {
			// 时间串带空格，必须保留引号，否则经 adb 拼接后会被设备 shell 拆成两个参数。
			args = append(args, "-T", "'"+since+"'")
		}
	} else {
		args = append(args, "logcat", "-d", "-v", "threadtime", "-t", strconv.Itoa(maxLines))
	}

	if app := strings.TrimSpace(in.App); app != "" {
		pkg, err := r.resolvePackage(p, in.ConnectKey, app)
		if err != nil {
			return fail("%v", err)
		}
		pid := pidOf(in.ConnectKey, pkg)
		if pid <= 0 {
			return fail("应用 %s 当前未运行，无法按进程过滤日志；请先调用 launch_app 启动它", pkg)
		}
		out.PackageName, out.Pid = pkg, pid
		args = append(args, "--pid="+strconv.Itoa(pid))
	}

	if tag := strings.TrimSpace(in.Tag); tag != "" {
		args = append(args, fmt.Sprintf("%s:%s", tag, level), "*:S")
	} else if level != "V" {
		args = append(args, fmt.Sprintf("*:%s", level))
	}

	execTimeout := 20 * time.Second
	if duration > 0 {
		execTimeout = time.Duration(duration)*time.Millisecond + 15*time.Second
	}
	res, err := adb.ExecuteAdbWithTimeout(args, execTimeout)
	if res == nil {
		return fail("读取 logcat 失败: 无返回")
	}
	// 采样模式下 timeout 到点会以 124 退出，这是预期收尾方式，只要拿到输出就继续。
	if err != nil && strings.TrimSpace(res.Output) == "" {
		return fail("读取 logcat 失败: %v", err)
	}

	raw := 0
	for _, line := range strings.Split(res.Output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 去掉 adb 自身打印的命令回显、以及 logcat 的分区横幅（"--------- beginning of main"）。
		if strings.HasPrefix(line, "[ADB]") || strings.HasPrefix(line, "--------- beginning of") {
			continue
		}
		raw++
		if re != nil && !re.MatchString(line) {
			continue
		}
		out.Lines = append(out.Lines, line)
	}
	out.RawLineCount = raw
	// 采样窗口内日志可能上万行（应用日志密集时 10 秒就过万），只保留窗口末尾 maxLines 条：
	// 越靠后的日志越接近"当前状态"，对排查问题更有价值。
	if duration > 0 && len(out.Lines) > maxLines {
		out.Lines = out.Lines[len(out.Lines)-maxLines:]
		out.Truncated = true
	}
	out.LineCount = len(out.Lines)
	// 缓冲模式由命令自身限定条数（logcat -d -t N），拿不到"缓冲区共多少行"，
	// 因此不能声称"被截断"——那会让模型以为漏了日志而反复调大 maxLines。
	if duration == 0 {
		out.Truncated = false
	}
	if out.Truncated {
		out.Hint = fmt.Sprintf("窗口内共 %d 行，仅返回最后 %d 行；可提高 level（如 I/W/E）、"+
			"用 tag/filter 缩小范围，或调低 durationMs", out.RawLineCount, out.LineCount)
	} else if duration == 0 && out.LineCount >= maxLines {
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
			"hint":         "没有匹配的日志。可尝试降低 level（如 V）、去掉 filter/tag，或操作应用后再读取。",
		})
	}
	return okJSON(out)
}

// ---------- 内部辅助 ----------

// resolvePackage 把"包名或包名片段"解析为唯一包名。
//
// 解析顺序：精确命中（忽略大小写）→ 唯一子串命中 → 报错列出候选。
//
// 这里刻意不采用"含 '.' 就当作完整包名"的捷径：鸿蒙的 bundleName 同样带点，
// 用户传 `hmos.settings`（com.huawei.hmos.settings 的尾部片段）时会被误当成
// 完整包名，`aa start -b hmos.settings` 直接报 "ability does not exist"，
// 而真正的原因是包名根本没解析——错误信息会把人引向 ability 名不匹配。
//
// 子串命中多个时报错，不猜：猜错会启动到别的应用，比失败更难排查。
// 完全匹配不到且输入含 '.' 时仍回退为原样返回，交给底层命令校验
// （覆盖系统隐藏包、未列在 bm/pm 列表里的包）。
func (r *Runtime) resolvePackage(p device.Platform, connectKey, app string) (string, error) {
	app = strings.TrimSpace(app)
	if app == "" {
		return "", fmt.Errorf("app 不能为空，请传应用包名（可用 list_apps 查询）")
	}

	names, err := r.installedPackages(p, connectKey)
	if err != nil {
		// 列表拿不到时不要阻断调用：含点的输入大概率就是完整包名，交给底层校验。
		if strings.Contains(app, ".") {
			return app, nil
		}
		return "", err
	}

	lower := strings.ToLower(app)
	for _, name := range names {
		if strings.ToLower(name) == lower {
			return name, nil
		}
	}

	var matches []string
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), lower) {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if strings.Contains(app, ".") {
			return app, nil
		}
		return "", fmt.Errorf("未找到包名包含 %q 的应用；请先调用 list_apps 查询，或直接传完整包名", app)
	default:
		shown := matches
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return "", fmt.Errorf("匹配到 %d 个应用，请指定完整包名，候选: %s",
			len(matches), strings.Join(shown, ", "))
	}
}

// installedPackages 按平台返回已安装应用包名列表。
func (r *Runtime) installedPackages(p device.Platform, connectKey string) ([]string, error) {
	var names []string
	if p == device.PlatformHarmonyOS {
		list, err := hdc.GetAppList(connectKey, "all")
		if err != nil {
			return nil, fmt.Errorf("解析应用包名失败: %v", err)
		}
		for _, a := range list.Apps {
			names = append(names, a.PackageName)
		}
		return names, nil
	}
	list, err := adb.GetAppList(connectKey, "all")
	if err != nil {
		return nil, fmt.Errorf("解析应用包名失败: %v", err)
	}
	for _, a := range list.Apps {
		names = append(names, a.PackageName)
	}
	return names, nil
}

// pngSize 从 PNG 文件头 IHDR chunk 解析真实宽高（解析失败返回 0,0）。
// PNG 签名 8 字节 + IHDR 长度/类型 8 字节后，依次是 4 字节宽、4 字节高（大端）。
func pngSize(data []byte) (int, int) {
	if len(data) < 24 || string(data[12:16]) != "IHDR" {
		return 0, 0
	}
	return int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19]),
		int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
}

// deviceTime 返回设备当前时间（logcat -T 可接受的 "YYYY-MM-DD HH:MM:SS.mmm" 格式）。
// 取设备时钟而不是本机时钟：两者可能有偏差，采样窗口必须以设备为准。
func deviceTime(connectKey string) string {
	res, err := adb.ExecuteAdbWithTimeout(
		[]string{"-s", connectKey, "shell", "date", `+"%Y-%m-%d %H:%M:%S.000"`}, 10*time.Second)
	if err != nil || res == nil || !res.Success {
		return ""
	}
	ts := strings.TrimSpace(res.Output)
	// 格式不符时宁可不加 -T，也不要把 logcat 弄失败。
	if len(ts) != len("2006-01-02 15:04:05.000") {
		return ""
	}
	return ts
}

// pidOf 返回应用主进程 pid（未运行返回 0）。
func pidOf(connectKey, pkg string) int {
	res, err := adb.ExecuteAdbWithTimeout(
		[]string{"-s", connectKey, "shell", "pidof", pkg}, 10*time.Second)
	if err != nil || res == nil || !res.Success {
		return 0
	}
	for _, f := range strings.Fields(res.Output) {
		if pid, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// foregroundActivity 返回当前前台 Activity（形如 com.tencent.mm/.ui.LauncherUI）。
func foregroundActivity(connectKey string) string {
	res, err := adb.ExecuteAdbWithTimeout(
		[]string{"-s", connectKey, "shell", "dumpsys", "activity", "activities"}, 15*time.Second)
	if err != nil || res == nil || !res.Success {
		return ""
	}
	for _, line := range strings.Split(res.Output, "\n") {
		// Android 10+ 有 topResumedActivity，低版本为 mResumedActivity。
		if !strings.Contains(line, "ResumedActivity") {
			continue
		}
		// 形如：mResumedActivity: ActivityRecord{... u0 com.pkg/.Act t123}
		if i := strings.Index(line, "u0 "); i >= 0 {
			rest := line[i+3:]
			if j := strings.IndexAny(rest, " }"); j > 0 {
				return rest[:j]
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
