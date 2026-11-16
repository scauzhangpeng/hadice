package hdc

// 鸿蒙设备输入注入原语。
//
// 设备侧的 `uitest uiInput` 提供 click/doubleClick/longClick/swipe/drag/fling/
// dircFling/keyEvent/inputText/text 等子命令，是鸿蒙上**不依赖投屏会话**即可
// 注入输入的唯一方式，语义等价于 Android 的 `adb shell input`。
//
// 与 backend/touch_events.go 的关系：GUI 侧优先走 hypium 手势通道（那是投屏
// 会话里的长连接），失败才退回 `uitest uiInput`。MCP 是无人值守调用，没有会话
// 可用，因此这里只封装 uitest 路径——两条路径最终落到同一条设备命令。

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// uiInputTimeout 是单次输入注入的超时。真机实测 click/swipe 均在百毫秒级，
// 10 秒只用于兜住设备卡死（如锁屏未解锁时 uitest 阻塞）。
const uiInputTimeout = 10 * time.Second

// Tap 在 (x, y) 点击一次。
func Tap(connectKey string, x, y int) (*HdcResult, error) {
	return uiInput(connectKey, "click", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
}

// DoubleTap 在 (x, y) 双击。
func DoubleTap(connectKey string, x, y int) (*HdcResult, error) {
	return uiInput(connectKey, "doubleClick", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
}

// LongPress 在 (x, y) 长按。
func LongPress(connectKey string, x, y int) (*HdcResult, error) {
	return uiInput(connectKey, "longClick", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
}

// Swipe 从 (fromX, fromY) 滑到 (toX, toY)，durationMs 为期望时长（默认 300ms）。
//
// uitest 的 swipe 接收的是**速度**(px/s) 而不是时长，这里按
// 速度 = 距离 / 秒数 换算，并夹到设备接受的 200~40000 区间。
// 也就是说滑动时长只是近似——设备侧按像素速度执行，这才是 uitest 的真实语义。
func Swipe(connectKey string, fromX, fromY, toX, toY, durationMs int) (*HdcResult, error) {
	if durationMs <= 0 {
		durationMs = 300
	}
	dist := math.Hypot(float64(toX-fromX), float64(toY-fromY))
	velocity := int(dist * 1000 / float64(durationMs))
	if velocity < 200 {
		velocity = 200
	}
	if velocity > 40000 {
		velocity = 40000
	}
	return uiInput(connectKey, "swipe",
		fmt.Sprintf("%d", fromX), fmt.Sprintf("%d", fromY),
		fmt.Sprintf("%d", toX), fmt.Sprintf("%d", toY),
		fmt.Sprintf("%d", velocity))
}

// Fling 按方向快速滑动（0/1/2/3 = 左/右/上/下），用于列表快速翻页。
func Fling(connectKey string, direction, velocity int) (*HdcResult, error) {
	if direction < 0 || direction > 3 {
		return nil, fmt.Errorf("direction 取值应为 0(左)/1(右)/2(上)/3(下)，收到 %d", direction)
	}
	if velocity < 200 {
		velocity = 600
	}
	if velocity > 40000 {
		velocity = 40000
	}
	return uiInput(connectKey, "dircFling", fmt.Sprintf("%d", direction), fmt.Sprintf("%d", velocity))
}

// PressKey 发送按键。key 支持 Back / Home / Power 三个具名键，
// 或直接传鸿蒙 KeyCode 数字（如 2054 = 返回）。
func PressKey(connectKey, key string) (*HdcResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("key 不能为空，可选 Back/Home/Power 或数字 KeyCode")
	}
	switch strings.ToLower(key) {
	case "back":
		key = "Back"
	case "home":
		key = "Home"
	case "power", "sleep":
		key = "Power"
	}
	return uiInput(connectKey, "keyEvent", key)
}

// InputText 向当前已获得焦点的输入框输入文本。
//
// 文本经设备 shell 传递，空格和引号会被重新切词，因此这里做单引号包裹
// （并把内部的单引号转成 '\'' 形式），保证多词文本作为单个参数到达 uitest。
func InputText(connectKey, text string) (*HdcResult, error) {
	if text == "" {
		return nil, fmt.Errorf("text 不能为空")
	}
	return uiInput(connectKey, "text", shellQuote(text))
}

// InputTextAt 先点击 (x, y) 再输入文本，用于坐标定位的输入框。
func InputTextAt(connectKey string, x, y int, text string) (*HdcResult, error) {
	if text == "" {
		return nil, fmt.Errorf("text 不能为空")
	}
	return uiInput(connectKey, "inputText",
		fmt.Sprintf("%d", x), fmt.Sprintf("%d", y), shellQuote(text))
}

// shellQuote 用单引号包裹参数，内层单引号替换为 '\''。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// uiInput 执行 `uitest uiInput <sub> <args...>`。
//
// uitest 成功时不打印任何内容、失败时把原因打成一行（如
// "Missing parameter." / "error: ..."），因此不能只看 hdc 的退出码：
// 有些机型上参数错误仍返回 0，必须再看输出内容。
func uiInput(connectKey, sub string, args ...string) (*HdcResult, error) {
	cmd := append([]string{"-t", connectKey, "shell", "uitest", "uiInput", sub}, args...)
	res, err := ExecuteHdcWithTimeout(cmd, uiInputTimeout)
	if err != nil {
		return &HdcResult{Success: false, Error: err.Error()}, err
	}
	if res == nil {
		return &HdcResult{Success: false, Error: "hdc 无返回"}, fmt.Errorf("hdc 无返回")
	}
	out := strings.TrimSpace(res.Output)
	if !res.Success || isUitestError(out) {
		msg := out
		if msg == "" {
			msg = strings.TrimSpace(res.Error)
		}
		if msg == "" {
			msg = "uitest 执行失败"
		}
		return &HdcResult{Success: false, Output: out, Error: msg}, fmt.Errorf("%s", msg)
	}
	return &HdcResult{Success: true, Output: out}, nil
}

// isUitestError 判断 uitest 输出是否代表失败。
//
// 注意：uitest **成功时也会打印 "No Error"**，这是唯一"含 error 但成功"的
// 形态，必须先剔除再找错误标记，否则每次点击都会被判成失败。
func isUitestError(out string) bool {
	if out == "" {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(out))
	low = strings.ReplaceAll(low, "no error", "")
	for _, marker := range []string{"missing parameter", "error", "usage", "fail", "not support"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
