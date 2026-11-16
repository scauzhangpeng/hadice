package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerDeviceTools 注册设备域工具。
// 后续的 device_info / device_screenshot / device_tap / device_logcat 等
// 设备域工具同样放在本文件（过大时再拆分），并在此追加注册。
func (r *Runtime) registerDeviceTools(s *mcp.Server) {
	registerTool(r, s, &mcp.Tool{
		Name: "list_devices",
		Description: "列出当前已连接的移动设备（鸿蒙 hdc + 安卓 adb 双平台并行枚举）。" +
			"返回每台设备的 connectKey、platform（harmonyos/android）、名称、型号、" +
			"连接类型（USB/TCP）与状态。本服务的其他工具均以 connectKey 定位设备，" +
			"取自本工具的返回结果。",
		Annotations: &mcp.ToolAnnotations{
			Title:         "列出已连接设备",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false), // 仅读本机 adb/hdc，不访问外部网络
		},
	}, r.listDevices)

	registerTool(r, s, &mcp.Tool{
		Name: "device_info",
		Description: "获取指定设备的详细静态信息：型号、品牌/厂商、设备形态、系统版本" +
			"（OS 版本/API 版本/安全补丁/内核）、CPU ABI 等。connectKey 取自 " +
			"list_devices 的返回结果。",
		Annotations: &mcp.ToolAnnotations{
			Title:         "获取设备详细信息",
			ReadOnlyHint:  true,
			OpenWorldHint: boolPtr(false),
		},
	}, r.deviceInfo)
}

type listDevicesIn struct {
	// Verbose 为 true 时 provider 会附带更多原始信息（多几次设备查询，耗时略长）。
	Verbose bool `json:"verbose,omitempty" jsonschema:"是否返回详细信息，默认 false"`
}

func (r *Runtime) listDevices(_ context.Context, _ *mcp.CallToolRequest, in listDevicesIn) (*mcp.CallToolResult, struct{}, error) {
	devices, err := r.manager.ListAllDevices(in.Verbose)
	if err != nil {
		return fail("获取设备列表失败: %v", err)
	}
	if len(devices) == 0 {
		return okText("未检测到已连接设备。请确认：1) 设备已通过 USB 连接并开启开发者模式；" +
			"2) 安卓设备已在手机上授权 adb 调试；3) 鸿蒙设备已允许 hdc 连接。")
	}
	return okJSON(devices)
}

type deviceInfoIn struct {
	// ConnectKey 定位目标设备（安卓为 serial，鸿蒙为 connect key）。
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
}

func (r *Runtime) deviceInfo(_ context.Context, _ *mcp.CallToolRequest, in deviceInfoIn) (*mcp.CallToolResult, struct{}, error) {
	if in.ConnectKey == "" {
		return fail("connectKey 不能为空，请先调用 list_devices 获取")
	}
	p := r.PlatformFor(in.ConnectKey)
	if p == "" {
		return fail("未找到设备 %s，请先调用 list_devices 确认设备已连接", in.ConnectKey)
	}
	provider := r.manager.GetProvider(p)
	if provider == nil {
		return fail("平台 %s 的 provider 不可用", p)
	}
	info, err := provider.GetDeviceDetailInfo(in.ConnectKey)
	if err != nil {
		return fail("获取设备 %s 的详细信息失败: %v", in.ConnectKey, err)
	}
	return okJSON(info)
}
