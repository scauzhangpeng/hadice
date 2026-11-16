package mcpserver

// 平台分派：同一批 MCP 工具同时覆盖 Android（adb）与 HarmonyOS（hdc）。
//
// 两个平台的设备操作在语义上是对应的，只是命令不同：
//
//	操作        Android                 HarmonyOS
//	截图        adb exec-out screencap  snapshot_display / uitest screenCap
//	点击        adb shell input tap     uitest uiInput click
//	滑动        adb shell input swipe   uitest uiInput swipe（按速度）
//	按键        adb shell input keyevent uitest uiInput keyEvent
//	应用列表    pm list packages        bm dump -a -l
//	启动应用    am start                aa start -a <ability> -b <bundle>
//	清数据      pm clear                bm clean -d -n
//	日志        logcat                  hilog
//
// 因此这里不另起一套工具，而是在 handler 入口按平台分派到各自的实现，
// 保持 12 个工具的名字/参数/返回结构在两个平台上完全一致——模型不需要
// 知道背后是 adb 还是 hdc，只要按 list_devices 给出的 connectKey 调用即可。

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"Hadice/backend/adb"
	"Hadice/backend/device"
	"Hadice/backend/hdc"
)

// requireDevice 校验 connectKey 对应当前已连接的设备，并返回其平台。
//
// 与原先 requireAndroid 的区别：不再把"不是 Android"当成工具不可用，
// 而是把平台交给调用方分派。
func (r *Runtime) requireDevice(connectKey string) (device.Platform, error) {
	if strings.TrimSpace(connectKey) == "" {
		return "", fmt.Errorf("connectKey 不能为空，请先调用 list_devices 获取")
	}
	p := r.PlatformFor(connectKey)
	if p == "" {
		return "", fmt.Errorf("未找到设备 %s，请先调用 list_devices 确认设备已连接", connectKey)
	}
	return p, nil
}

// screenSize 是设备屏幕的逻辑分辨率（用于校验输入坐标）。
type screenSize struct {
	width  int
	height int
	at     time.Time
}

// screenSizeTTL 是屏幕尺寸缓存的有效期。分辨率只在投屏改 Override size 或
// 横竖屏切换时变化，短缓存既避免每次点击都多跑一条命令，又能跟上变化。
const screenSizeTTL = 5 * time.Minute

// screenSizeOf 返回设备当前分辨率（取不到返回 0,0，调用方据此跳过校验）。
func (r *Runtime) screenSizeOf(connectKey string, p device.Platform) (int, int) {
	r.devMu.Lock()
	if sz, ok := r.sizes[connectKey]; ok && time.Since(sz.at) < screenSizeTTL {
		r.devMu.Unlock()
		return sz.width, sz.height
	}
	r.devMu.Unlock()

	width, height := queryScreenSize(connectKey, p)
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	r.devMu.Lock()
	if r.sizes == nil {
		r.sizes = make(map[string]screenSize)
	}
	r.sizes[connectKey] = screenSize{width: width, height: height, at: time.Now()}
	r.devMu.Unlock()
	return width, height
}

// queryScreenSize 按平台取屏幕分辨率。
//
//	Android：wm size，Override size 优先（投屏改过分辨率时物理分辨率不可用）
//	HarmonyOS：hidumper RenderService，取 screen[0] 的 render resolution
func queryScreenSize(connectKey string, p device.Platform) (int, int) {
	if p == device.PlatformHarmonyOS {
		res, err := hdc.ExecuteHdcWithTimeout(
			[]string{"-t", connectKey, "shell", "hidumper", "-s", "RenderService", "-a", "screen"}, 10*time.Second)
		if err != nil || res == nil || !res.Success {
			return 0, 0
		}
		for _, line := range strings.Split(res.Output, "\n") {
			if !strings.Contains(line, "screen[0]") {
				continue
			}
			if m := resolutionPattern.FindStringSubmatch(line); len(m) == 3 {
				w, _ := strconv.Atoi(m[1])
				h, _ := strconv.Atoi(m[2])
				return w, h
			}
		}
		return 0, 0
	}

	res, err := adb.ExecuteAdbWithTimeout(
		[]string{"-s", connectKey, "shell", "wm", "size"}, 10*time.Second)
	if err != nil || res == nil || !res.Success {
		return 0, 0
	}
	override, physical := 0, 0
	for _, line := range strings.Split(res.Output, "\n") {
		line = strings.TrimSpace(line)
		m := resolutionPattern.FindStringSubmatch(line)
		if len(m) != 3 {
			continue
		}
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		switch {
		case strings.HasPrefix(strings.ToLower(line), "override"):
			override = w*10000 + h // 编码成单个整数方便后面二选一
		case strings.HasPrefix(strings.ToLower(line), "physical"):
			physical = w*10000 + h
		}
	}
	if override != 0 {
		return override / 10000, override % 10000
	}
	if physical != 0 {
		return physical / 10000, physical % 10000
	}
	return 0, 0
}

// resolutionPattern 匹配 "1260x2720" / "Physical size: 1344x2772" 里的宽高。
var resolutionPattern = regexp.MustCompile(`(\d{3,5})\s*[x×]\s*(\d{3,5})`)

// hdcErrText 把 hdc 调用结果整理成一行错误摘要。// 去重规则是"子串包含即视为重复"：hdc 的失败信息在 err.Error()、res.Error、
// res.Output 里往往是同一段文本的不同包装（如 StartApp 把输出塞进 error 里），
// 按等值去重会得到三份一模一样的长文本。
//
// 同时限制长度：hdc 失败时常附整段 "Try the following:" 帮助，对模型是噪音。
func hdcErrText(res *hdc.HdcResult, err error) string {
	const maxRunes = 400
	var parts []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for i, p := range parts {
			if p == s || strings.Contains(p, s) {
				return
			}
			if strings.Contains(s, p) {
				parts[i] = s
				return
			}
		}
		parts = append(parts, s)
	}
	if err != nil {
		add(err.Error())
	}
	if res != nil {
		add(res.Error)
		add(res.Output)
	}
	if len(parts) == 0 {
		return "hdc 命令执行失败（无输出）"
	}
	msg := strings.Join(parts, " | ")
	// StartApp 等实现会把 "启动失败: " 前缀写进 error，调用方再包一层就重复了。
	msg = strings.TrimPrefix(msg, "启动失败: ")
	msg = strings.TrimPrefix(msg, "启动失败：")
	if runes := []rune(msg); len(runes) > maxRunes {
		msg = string(runes[:maxRunes]) + "…"
	}
	return msg
}
