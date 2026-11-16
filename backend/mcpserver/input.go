package mcpserver

// 输入域工具：坐标点击、滑动、返回键。
//
// 两个平台都走"设备侧注入"，不依赖投屏会话：
//   - Android：`adb shell input tap/swipe/keyevent`
//   - HarmonyOS：`uitest uiInput click/swipe/keyEvent`（见 backend/hdc/input.go）
//
// 为什么不用 GUI 侧的 Click / TouchMoveFromTo / PressBack：
// 那几个方法优先走 scrcpy 控制通道或 hypium 手势通道，而这两个通道只在
// "正在投屏该设备"时存在。MCP 是无人值守调用，不应要求用户先开着投屏，
// 因此直接用设备侧命令：语义等价、无会话依赖、调用时机不受限。
//
// 典型用法（配合 device_screenshot 形成操作闭环）：
//
//	device_screenshot → 看图确定目标坐标 → device_tap → device_screenshot 验证
//
// 对微信这类屏蔽 accessibility 的应用（uiautomator dump 返回空），
// 这条"看图—点击—复验"的链路是唯一可行的深页面导航方式。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"Hadice/backend/adb"
	"Hadice/backend/device"
	"Hadice/backend/hdc"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerInputTools 注册输入域工具。
func (r *Runtime) registerInputTools(s *mcp.Server) {
	registerTool(r, s, &mcp.Tool{
		Name: "device_tap",
		Description: "点击设备屏幕上的指定坐标（像素）。坐标可先用 device_screenshot 截图，" +
			"按图片实际分辨率折算。用于进入应用内的深层页面（如微信朋友圈）或确认弹窗。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "点击屏幕坐标",
			ReadOnlyHint:    false,
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(false),
		},
	}, r.deviceTap)

	registerTool(r, s, &mcp.Tool{
		Name: "device_swipe",
		Description: "从起点滑动到终点（像素），durationMs 控制速度（默认 300ms）。" +
			"用于列表滚动、翻页、下拉刷新等；快速短滑可用于 fling。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "滑动屏幕",
			ReadOnlyHint:    false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(false),
		},
	}, r.deviceSwipe)

	registerTool(r, s, &mcp.Tool{
		Name: "device_back",
		Description: "发送返回键（Android KEYCODE_BACK / 鸿蒙 keyEvent Back），用于退出当前页面/关闭弹窗。" +
			"返回后可再调用 device_screenshot 确认界面。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "返回上一页",
			ReadOnlyHint:    false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(false),
		},
	}, r.deviceBack)
}

type tapIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	X          int    `json:"x" jsonschema:"横坐标（像素），以截图实际宽度为准"`
	Y          int    `json:"y" jsonschema:"纵坐标（像素），以截图实际高度为准"`
}

func (r *Runtime) deviceTap(_ context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	if in.X < 0 || in.Y < 0 {
		return fail("坐标不能为负数，收到 (%d, %d)", in.X, in.Y)
	}
	if err := r.checkPoint(p, in.ConnectKey, in.X, in.Y); err != nil {
		return fail("%v", err)
	}
	if p == device.PlatformHarmonyOS {
		res, err := hdc.Tap(in.ConnectKey, in.X, in.Y)
		if err != nil {
			return fail("点击 (%d, %d) 失败: %s", in.X, in.Y, hdcErrText(res, err))
		}
		return okJSON(map[string]any{"action": "tap", "x": in.X, "y": in.Y, "success": true})
	}
	if out, err := runInput(in.ConnectKey, "tap", fmt.Sprintf("%d", in.X), fmt.Sprintf("%d", in.Y)); err != nil {
		return fail("点击 (%d, %d) 失败: %v%s", in.X, in.Y, err, out)
	}
	return okJSON(map[string]any{"action": "tap", "x": in.X, "y": in.Y, "success": true})
}

type swipeIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	FromX      int    `json:"fromX" jsonschema:"起点横坐标（像素）"`
	FromY      int    `json:"fromY" jsonschema:"起点纵坐标（像素）"`
	ToX        int    `json:"toX" jsonschema:"终点横坐标（像素）"`
	ToY        int    `json:"toY" jsonschema:"终点纵坐标（像素）"`
	// DurationMs 滑动时长，默认 300ms；越小越快（越接近 fling）。
	DurationMs int `json:"durationMs,omitempty" jsonschema:"滑动时长毫秒，默认 300，范围 50-5000"`
}

func (r *Runtime) deviceSwipe(_ context.Context, _ *mcp.CallToolRequest, in swipeIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	if in.FromX < 0 || in.FromY < 0 || in.ToX < 0 || in.ToY < 0 {
		return fail("坐标不能为负数")
	}
	if in.FromX == in.ToX && in.FromY == in.ToY {
		return fail("起点与终点相同，无法滑动")
	}
	if err := r.checkPoint(p, in.ConnectKey, in.FromX, in.FromY); err != nil {
		return fail("%v", err)
	}
	if err := r.checkPoint(p, in.ConnectKey, in.ToX, in.ToY); err != nil {
		return fail("%v", err)
	}
	dur := in.DurationMs
	if dur <= 0 {
		dur = 300
	}
	if dur < 50 {
		dur = 50
	}
	if dur > 5000 {
		dur = 5000
	}
	if p == device.PlatformHarmonyOS {
		// 鸿蒙侧 uitest 按速度执行，durationMs 会被换算成 px/s（见 hdc.Swipe）。
		res, err := hdc.Swipe(in.ConnectKey, in.FromX, in.FromY, in.ToX, in.ToY, dur)
		if err != nil {
			return fail("滑动失败: %s", hdcErrText(res, err))
		}
		return okJSON(map[string]any{
			"action": "swipe", "fromX": in.FromX, "fromY": in.FromY,
			"toX": in.ToX, "toY": in.ToY, "durationMs": dur, "success": true,
		})
	}
	args := []string{
		fmt.Sprintf("%d", in.FromX), fmt.Sprintf("%d", in.FromY),
		fmt.Sprintf("%d", in.ToX), fmt.Sprintf("%d", in.ToY),
		fmt.Sprintf("%d", dur),
	}
	if out, err := runInput(in.ConnectKey, "swipe", args...); err != nil {
		return fail("滑动失败: %v%s", err, out)
	}
	return okJSON(map[string]any{
		"action": "swipe", "fromX": in.FromX, "fromY": in.FromY,
		"toX": in.ToX, "toY": in.ToY, "durationMs": dur, "success": true,
	})
}

type backIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
}

func (r *Runtime) deviceBack(_ context.Context, _ *mcp.CallToolRequest, in backIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	if p == device.PlatformHarmonyOS {
		res, err := hdc.PressKey(in.ConnectKey, "Back")
		if err != nil {
			return fail("发送返回键失败: %s", hdcErrText(res, err))
		}
		return okJSON(map[string]any{"action": "back", "key": "Back", "success": true})
	}
	if out, err := runInput(in.ConnectKey, "keyevent", "4"); err != nil {
		return fail("发送返回键失败: %v%s", err, out)
	}
	return okJSON(map[string]any{"action": "back", "key": "Back", "success": true})
}

// checkPoint 校验坐标是否落在屏幕内。
//
// 两个平台的 input 命令都不做越界检查：adb 的 `input tap 99999 99999` 会
// 静默成功（什么都没点），鸿蒙的 uitest 同样返回 "No Error"。对模型来说
// "成功但界面没变化"是最难排查的一种失败，因此在入口直接拦掉。
// 取不到分辨率（如设备刚连接、RenderService 不可用）时放行，不阻断正常调用。
func (r *Runtime) checkPoint(p device.Platform, connectKey string, x, y int) error {
	w, h := r.screenSizeOf(connectKey, p)
	if w <= 0 || h <= 0 {
		return nil
	}
	if x >= w || y >= h {
		return fmt.Errorf("坐标 (%d, %d) 超出屏幕范围，当前分辨率 %dx%d：屏幕坐标从 (0,0) 到 (%d,%d)，"+
			"通常来自截图里读到的坐标，请确认用的是设备像素而不是显示像素",
			x, y, w, h, w-1, h-1)
	}
	return nil
}

// runInput 执行 `adb shell input <args...>`，返回输出片段用于错误提示。
func runInput(connectKey, sub string, args ...string) (string, error) {
	cmd := append([]string{"-s", connectKey, "shell", "input", sub}, args...)
	res, err := adb.ExecuteAdbWithTimeout(cmd, 10*time.Second)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", fmt.Errorf("无返回")
	}
	out := strings.TrimSpace(res.Output)
	if !res.Success {
		return out, fmt.Errorf("命令执行失败")
	}
	return out, nil
}
