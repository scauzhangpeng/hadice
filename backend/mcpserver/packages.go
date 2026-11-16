package mcpserver

// 包管理域工具：安装包、卸载、清除应用数据。
//
// 这三个工具是「改代码 → 装包 → 验收」闭环的起点：
//
//	install_app → launch_app → device_screenshot / device_logcat
//	uninstall_app / clear_app_data   用于验证首次启动、引导页、权限弹窗
//
// 两个平台各自的等价命令：
//
//	操作     Android                    HarmonyOS
//	安装     adb install -r (-t)        hdc install <hap> -r
//	卸载     adb uninstall              bm uninstall -n
//	清数据   pm clear                   bm clean -d -n
//
// 命令语义不同但"结果"相同，因此工具入口保持同一套参数与返回结构，
// 由 handler 按 list_devices 的 platform 分派；安装包类型也随之不同
// （Android 收 .apk，鸿蒙收 .hap/.app）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"Hadice/backend/adb"
	"Hadice/backend/device"
	"Hadice/backend/hdc"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerPackageTools 注册包管理域工具。
func (r *Runtime) registerPackageTools(s *mcp.Server) {
	registerTool(r, s, &mcp.Tool{
		Name: "install_app",
		Description: "把本机安装包安装到设备：Android 收 .apk（等价 adb install -r，覆盖安装并保留数据），" +
			"鸿蒙收 .hap/.app（等价 hdc install -r）。返回安装前后的版本号，用于确认装上去的是新构建。" +
			"apkPath 传本机绝对路径；fresh=true 会先卸载已装版本再安装，用于验证首次启动/引导页/权限弹窗。" +
			"安装成功后通常接 launch_app → device_screenshot / device_logcat。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "安装 APK",
			ReadOnlyHint:    false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(true),
		},
	}, r.installApp)

	registerTool(r, s, &mcp.Tool{
		Name: "uninstall_app",
		Description: "从设备卸载指定应用（Android 等价 adb uninstall，鸿蒙等价 bm uninstall，应用数据一并删除）。" +
			"app 传包名或包名片段，只有中文应用名时先用 list_apps 查包名。系统关键包会被拒绝；" +
			"常与 install_app 搭配验证全新安装。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "卸载应用",
			ReadOnlyHint:    false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(true),
		},
	}, r.uninstallApp)

	registerTool(r, s, &mcp.Tool{
		Name: "clear_app_data",
		Description: "清除应用数据（Android 等价 pm clear，鸿蒙等价 bm clean -d）：登录态、缓存、本地配置一并清空。" +
			"用于复现「首次启动」「未登录」场景，之后接 launch_app 即可从头走一遍。" +
			"只清数据不卸载，比 uninstall_app + install_app 快。",
		Annotations: &mcp.ToolAnnotations{
			Title:           "清除应用数据",
			ReadOnlyHint:    false,
			OpenWorldHint:   boolPtr(false),
			DestructiveHint: boolPtr(true),
		},
	}, r.clearAppData)
}

// ---------- install_app ----------

type installAppIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	// ApkPath 本机安装包绝对路径（App 的 cwd 不固定，相对路径不可靠）。
	ApkPath string `json:"apkPath" jsonschema:"本机安装包绝对路径：Android 传 .apk，鸿蒙传 .hap/.app，如 /Users/me/proj/entry/build/default/outputs/default/entry-default-signed.hap"`
	// PackageName 可选：APK 解析不出包名时用它读取安装后的版本信息。
	PackageName string `json:"packageName,omitempty" jsonschema:"已知道包名时可直接传，用于读取安装后的版本号（APK 解析失败时作兜底）"`
	// Fresh 为 true 时先卸载再安装，等价"全新安装"。
	Fresh bool `json:"fresh,omitempty" jsonschema:"是否先卸载已装版本再安装（验证首次启动），默认 false 即覆盖安装"`
}

type installResult struct {
	PackageName       string `json:"packageName,omitempty"`
	ApkPath           string `json:"apkPath"`
	ApkSizeBytes      int64  `json:"apkSizeBytes,omitempty"`
	ApkVersionName    string `json:"apkVersionName,omitempty"`
	ApkVersionCode    int    `json:"apkVersionCode,omitempty"`
	Fresh             bool   `json:"fresh"`
	BeforeVersionName string `json:"beforeVersionName,omitempty"`
	BeforeVersionCode int    `json:"beforeVersionCode,omitempty"`
	VersionName       string `json:"versionName,omitempty"`
	VersionCode       int    `json:"versionCode,omitempty"`
	ElapsedMs         int64  `json:"elapsedMs"`
	Output            string `json:"output,omitempty"`
	Hint              string `json:"hint,omitempty"`
}

func (r *Runtime) installApp(_ context.Context, _ *mcp.CallToolRequest, in installAppIn) (*mcp.CallToolResult, struct{}, error) {
	// 先校验本机路径（纯本地、瞬时），再查设备：路径写错时不该被
	// "设备未连接" 掩盖，否则要修两轮才能装上。
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	pkgPath, size, err := normalizePackagePath(p, in.ApkPath)
	if err != nil {
		return fail("%v", err)
	}

	out := installResult{ApkPath: pkgPath, ApkSizeBytes: size, Fresh: in.Fresh}
	if p == device.PlatformHarmonyOS {
		return r.installHap(in, pkgPath, out)
	}

	// 先从 APK 自身读包名与版本（aapt，与 GUI 的"APK 预览"同一实现），
	// 解析不出来再退回调用方给的包名——只影响版本回显，不阻断安装。
	if info, perr := adb.ParseApkAppInfo(pkgPath); perr == nil && info != nil {
		out.PackageName = info.BundleName
		out.ApkVersionName = info.VersionName
		out.ApkVersionCode = info.VersionCode
	}
	if out.PackageName == "" {
		out.PackageName = strings.TrimSpace(in.PackageName)
	}
	if out.PackageName != "" {
		if before, berr := adb.GetAppDetail(in.ConnectKey, out.PackageName); berr == nil && before != nil {
			out.BeforeVersionName, out.BeforeVersionCode = before.Version, before.VersionCode
		}
	}

	start := time.Now()
	if in.Fresh {
		if out.PackageName == "" {
			return fail("fresh=true 需要先知道包名（用于卸载旧版本）：APK 解析失败时请同时传 packageName")
		}
		// 未安装时 uninstall 会失败，这属于预期（本来就没有旧版本），忽略。
		_, _ = adb.UninstallApp(in.ConnectKey, out.PackageName)
		time.Sleep(500 * time.Millisecond)
	}

	res, ierr := adb.InstallApp(in.ConnectKey, pkgPath, !in.Fresh)
	out.ElapsedMs = time.Since(start).Milliseconds()
	if res == nil {
		return fail("安装失败：adb 无返回（%v）", ierr)
	}
	out.Output = tailText(strings.TrimSpace(res.Output+"\n"+res.Error), 1200)
	if ierr != nil || !res.Success {
		hint := installFailureHint(res.Output + " " + res.Error)
		if hint == "" {
			hint = "adb 安装失败，详见输出"
		}
		return fail("安装 %s 失败：%s\n\nadb 输出:\n%s", filepath.Base(pkgPath), hint, out.Output)
	}

	// 读安装后的真实版本：这是"装的是不是新构建"的唯一可靠依据。
	if out.PackageName != "" {
		if after, aerr := adb.GetAppDetail(in.ConnectKey, out.PackageName); aerr == nil && after != nil {
			out.VersionName, out.VersionCode = after.Version, after.VersionCode
		}
	}
	out.Hint = installVersionHint(out)
	return okJSON(out)
}

// installHap 是 installApp 的鸿蒙分支：hdc install + bm dump 回显版本。
func (r *Runtime) installHap(in installAppIn, hapPath string, out installResult) (*mcp.CallToolResult, struct{}, error) {
	if info, perr := hdc.ParseHapAppInfo(hapPath); perr == nil && info != nil {
		out.PackageName = info.BundleName
		out.ApkVersionName = info.VersionName
		out.ApkVersionCode = info.VersionCode
	}
	if out.PackageName == "" {
		out.PackageName = strings.TrimSpace(in.PackageName)
	}
	if out.PackageName != "" {
		beforeName, beforeCode := hdc.AppVersion(in.ConnectKey, out.PackageName)
		out.BeforeVersionName, out.BeforeVersionCode = beforeName, beforeCode
	}

	start := time.Now()
	if in.Fresh {
		if out.PackageName == "" {
			return fail("fresh=true 需要先知道包名（用于卸载旧版本）：HAP 解析失败时请同时传 packageName")
		}
		_, _ = hdc.UninstallApp(in.ConnectKey, out.PackageName)
		time.Sleep(500 * time.Millisecond)
	}

	// replace=!fresh：覆盖安装保留数据，fresh 时已先卸载，无需 -r。
	res, ierr := hdc.InstallApp(in.ConnectKey, hapPath, !in.Fresh, false)
	out.ElapsedMs = time.Since(start).Milliseconds()
	if res != nil {
		out.Output = tailText(strings.TrimSpace(res.Output+"\n"+res.Error), 1200)
	}
	if ierr != nil || res == nil || !res.Success {
		hint := hapInstallFailureHint(out.Output)
		return fail("安装 %s 失败：%s\n\nhdc 输出:\n%s", filepath.Base(hapPath), hint, out.Output)
	}

	if out.PackageName != "" {
		out.VersionName, out.VersionCode = hdc.AppVersion(in.ConnectKey, out.PackageName)
	}
	out.Hint = installVersionHint(out)
	return okJSON(out)
}

// installVersionHint 根据"包内版本 vs 设备版本"给出结论性提示（两平台共用）。
func installVersionHint(out installResult) string {
	switch {
	case out.PackageName == "":
		return "已安装，但无法确定包名（安装包解析失败且未传 packageName），无法回显版本号；" +
			"可调用 list_apps 确认包名，或下次安装时传 packageName"
	case out.VersionName == "":
		return "已安装，但读取版本号失败；可调用 list_apps 确认应用状态"
	case out.ApkVersionCode > 0 && out.VersionCode > 0 && out.ApkVersionCode != out.VersionCode:
		return fmt.Sprintf("设备上的 versionCode(%d) 与安装包内的(%d) 不一致：可能装到了别的变体包，或安装未生效",
			out.VersionCode, out.ApkVersionCode)
	}
	return ""
}

// ---------- uninstall_app ----------

type uninstallAppIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	App        string `json:"app" jsonschema:"应用包名（如 com.tencent.mm）或包名片段（如 tencent.mm）"`
}

func (r *Runtime) uninstallApp(_ context.Context, _ *mcp.CallToolRequest, in uninstallAppIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	pkg, err := r.resolvePackage(p, in.ConnectKey, in.App)
	if err != nil {
		return fail("%v", err)
	}
	if reason, bad := protectedPackages[pkg]; bad {
		return fail("拒绝卸载 %s：%s。该操作会让设备不可正常使用。", pkg, reason)
	}
	if !isPackageInstalled(p, in.ConnectKey, pkg) {
		return fail("设备上未安装 %s，无需卸载（可用 list_apps 确认包名）", pkg)
	}

	if p == device.PlatformHarmonyOS {
		res, uerr := hdc.UninstallApp(in.ConnectKey, pkg)
		output := ""
		if res != nil {
			output = strings.TrimSpace(res.Output + "\n" + res.Error)
		}
		if uerr != nil || res == nil || !res.Success {
			return fail("卸载 %s 失败：%s\n\nhdc 输出:\n%s", pkg, hapUninstallFailureHint(output), tailText(output, 800))
		}
		stillInstalled := hdc.IsInstalled(in.ConnectKey, pkg)
		out := map[string]any{
			"packageName": pkg,
			"uninstalled": !stillInstalled,
			"output":      tailText(output, 400),
		}
		if stillInstalled {
			out["hint"] = "hdc 报成功但仍能查到该包：可能是系统预装应用（不可卸载），可用 list_apps 复核"
		}
		return okJSON(out)
	}

	res, uerr := adb.UninstallApp(in.ConnectKey, pkg)
	output := ""
	if res != nil {
		output = strings.TrimSpace(res.Output + "\n" + res.Error)
	}
	if uerr != nil || res == nil || !res.Success {
		return fail("卸载 %s 失败：%s\n\nadb 输出:\n%s", pkg, uninstallFailureHint(output), tailText(output, 800))
	}
	// 卸载命令返回 Success 后可能仍有残留（多用户/设备管理员场景），复查一次。
	stillInstalled := isPackageInstalled(p, in.ConnectKey, pkg)
	out := map[string]any{
		"packageName": pkg,
		"uninstalled": !stillInstalled,
		"output":      tailText(output, 400),
	}
	if stillInstalled {
		out["hint"] = "adb 报成功但仍能查到该包：可能只在当前用户下卸载，或系统预装包被还原；可用 list_apps 复核"
	}
	return okJSON(out)
}

// ---------- clear_app_data ----------

type clearAppDataIn struct {
	ConnectKey string `json:"connectKey" jsonschema:"目标设备的 connectKey，取自 list_devices 返回结果"`
	App        string `json:"app" jsonschema:"应用包名（如 com.tencent.mm）或包名片段（如 tencent.mm）"`
}

type clearAppDataResult struct {
	PackageName string `json:"packageName"`
	Cleared     bool   `json:"cleared"`
	WasRunning  bool   `json:"wasRunning"`
	Pid         int    `json:"pid,omitempty"`
	Output      string `json:"output,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

func (r *Runtime) clearAppData(_ context.Context, _ *mcp.CallToolRequest, in clearAppDataIn) (*mcp.CallToolResult, struct{}, error) {
	p, perr := r.requireDevice(in.ConnectKey)
	if perr != nil {
		return fail("%v", perr)
	}
	pkg, err := r.resolvePackage(p, in.ConnectKey, in.App)
	if err != nil {
		return fail("%v", err)
	}
	if reason, bad := protectedPackages[pkg]; bad {
		return fail("拒绝清除 %s 的数据：%s。该操作会让设备不可正常使用。", pkg, reason)
	}
	if !isPackageInstalled(p, in.ConnectKey, pkg) {
		return fail("设备上未安装 %s（可用 list_apps 确认包名）", pkg)
	}

	out := clearAppDataResult{PackageName: pkg}

	if p == device.PlatformHarmonyOS {
		out.Pid = hdc.PidOf(in.ConnectKey, pkg)
		out.WasRunning = out.Pid > 0
		res, cerr := hdc.ClearAppData(in.ConnectKey, pkg)
		if res != nil {
			out.Output = tailText(strings.TrimSpace(res.Output+"\n"+res.Error), 400)
		}
		// bm clean 成功时输出 "clean bundle data files successfully"，失败时为 "error"/"failed"。
		lowerOut := strings.ToLower(out.Output)
		out.Cleared = cerr == nil && res != nil && res.Success &&
			!strings.Contains(lowerOut, "error") && !strings.Contains(lowerOut, "fail")
		if !out.Cleared {
			out.Hint = "清除数据失败：" + hdcErrText(res, cerr) +
				"；预装/系统应用、被设为设备管理员的应用可能不允许清数据"
			return okJSON(out)
		}
		out.Hint = "数据已清空；下一次 launch_app 会走首次启动流程（引导页/权限弹窗/登录页）"
		return okJSON(out)
	}

	out.Pid = pidOf(in.ConnectKey, pkg)
	out.WasRunning = out.Pid > 0

	res, cerr := adb.ClearAppData(in.ConnectKey, pkg)
	if res != nil {
		out.Output = tailText(strings.TrimSpace(res.Output+"\n"+res.Error), 400)
	}
	// pm clear 成功时输出 "Success"；失败时进程未必非零退出，两者都要看。
	out.Cleared = cerr == nil && res != nil && strings.Contains(strings.ToLower(res.Output), "success")
	if !out.Cleared {
		out.Hint = "清除数据失败：应用可能被设为设备管理员、处于工作资料（/ 多用户）下，或已被冻结；" +
			"可尝试先 uninstall_app 再 install_app"
		if strings.Contains(strings.ToLower(out.Output), "failed") {
			out.Hint = "pm clear 返回 Failed：应用可能不存在、被设备管理员占用或受工作资料限制"
		}
		return okJSON(out)
	}
	out.Hint = "数据已清空，进程已结束；下一次 launch_app 会走首次启动流程（引导页/权限弹窗/登录页）"
	return okJSON(out)
}

// ---------- 内部辅助 ----------

// protectedPackages 是卸载/清数据后会让设备无法正常使用的包，一律拒绝。
// 两个平台的系统关键包都收在这里（鸿蒙侧是 bundleName）。
var protectedPackages = map[string]string{
	// Android
	"android":              "系统框架本身",
	"com.android.systemui": "系统界面（卸载后没有状态栏与导航栏）",
	"com.android.settings": "系统设置（卸载后无法进入设置）",
	"com.android.phone":    "电话服务（卸载后无法通话/上网鉴权）",
	"com.android.shell":    "adb shell 自身（卸载后调试通道失效）",
	// HarmonyOS
	"com.ohos.sceneboard":        "系统桌面（卸载后没有桌面与任务界面）",
	"com.ohos.systemui":          "系统界面（卸载后没有状态栏与导航栏）",
	"com.huawei.hmos.settings":   "系统设置（卸载后无法进入设置）",
	"com.ohos.settings":          "系统设置（卸载后无法进入设置）",
	"com.ohos.shell":             "hdc shell 自身（卸载后调试通道失效）",
	"com.huawei.hmos.aidataservice": "系统元服务（卸载后系统功能异常）",
}

// normalizePackagePath 校验并规范化本机安装包路径，返回绝对路径与文件大小。
// 允许的扩展名随平台变化：Android 收 .apk，鸿蒙收 .hap/.app。
func normalizePackagePath(p device.Platform, path string) (string, int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", 0, fmt.Errorf("apkPath 不能为空，请传本机安装包文件的绝对路径")
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", 0, fmt.Errorf("解析 apkPath 失败: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("找不到安装包文件 %s: %v", abs, err)
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("apkPath 指向目录，需要传安装包文件: %s", abs)
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if p == device.PlatformHarmonyOS {
		switch ext {
		case ".hap", ".app":
			return abs, info.Size(), nil
		case ".apk":
			return "", 0, fmt.Errorf("%s 是 Android APK，鸿蒙设备需要 .hap（单模块）或 .app（整包）", filepath.Base(abs))
		default:
			return "", 0, fmt.Errorf("不支持的文件类型 %q，鸿蒙设备需要 .hap 或 .app 文件: %s", ext, abs)
		}
	}
	switch ext {
	case ".apk":
	case ".apks", ".xapk", ".apkm":
		return "", 0, fmt.Errorf("%s 是分包/打包格式，暂不支持；请用 bundletool 生成 universal APK 后再安装",
			filepath.Base(abs))
	case ".hap", ".app":
		return "", 0, fmt.Errorf("%s 是鸿蒙安装包，Android 设备需要 .apk", filepath.Base(abs))
	default:
		return "", 0, fmt.Errorf("不支持的文件类型 %q，需要 .apk 文件: %s", ext, abs)
	}
	return abs, info.Size(), nil
}

// installFailureHint 把 adb install 的典型错误翻译成可执行的下一步。
func installFailureHint(output string) string {
	up := strings.ToUpper(output)
	for _, m := range []struct{ marker, hint string }{
		{"INSTALL_FAILED_VERSION_DOWNGRADE", "设备上已装版本更高（versionCode 更大），adb 拒绝降级；确需降级请加 fresh=true 先卸载"},
		{"INSTALL_FAILED_UPDATE_INCOMPATIBLE", "签名与设备上已装版本不一致（常见于换了签名/不同环境构建），需先卸载：加 fresh=true"},
		{"INSTALL_FAILED_ALREADY_EXISTS", "设备上已存在该应用，覆盖安装未生效（可能是多用户/工作资料下的副本）"},
		{"INSTALL_FAILED_INSUFFICIENT_STORAGE", "设备存储空间不足，清理后再试"},
		{"INSTALL_FAILED_USER_RESTRICTED", "设备限制了 USB 安装（开发者选项的「USB 安装」/MIUI 优化/企业策略），需在手机上允许"},
		{"INSTALL_FAILED_OLDER_SDK", "APK 的 minSdkVersion 高于设备系统版本，这个包装不了"},
		{"INSTALL_FAILED_DEPRECATED_SDK_VERSION", "APK 的 targetSdk 已不被当前系统接受"},
		{"INSTALL_FAILED_NO_MATCHING_ABIS", "APK 不含设备 CPU 架构的 so（如设备是 arm64-v8a 而包里只有 x86/armeabi）"},
		{"INSTALL_FAILED_TEST_ONLY", "APK 带 testOnly 标记（AndroidManifest 的 android:testOnly=true），需重新打包去掉"},
		{"INSTALL_PARSE_FAILED", "APK 解析失败：文件损坏或未正确签名（debug 包签名异常常见）"},
		{"INSTALL_FAILED_INVALID_APK", "APK 无效：打包不完整或文件被破坏"},
		{"INSTALL_CANCELED_BY_USER", "安装被手机端取消：需解锁屏幕并在弹窗里点「允许」"},
		{"INSTALL_REJECTED", "手机端拒绝了安装：检查「安装未知应用」权限或设备管理策略"},
	} {
		if strings.Contains(up, m.marker) {
			return m.hint
		}
	}
	return ""
}

// uninstallFailureHint 把 adb uninstall 的典型错误翻译成可执行的下一步。
func uninstallFailureHint(output string) string {
	up := strings.ToUpper(output)
	switch {
	case strings.Contains(up, "DELETE_FAILED_INTERNAL_ERROR"):
		return "系统预装应用（或由设备管理员/工作资料托管）无法直接卸载；可尝试在设备上手动停用"
	case strings.Contains(up, "DELETE_FAILED_DEVICE_POLICY_MANAGER"):
		return "该应用是设备管理员，需先在系统设置里取消设备管理"
	case strings.Contains(up, "DELETE_FAILED_USER_RESTRICTED"), strings.Contains(up, "USER_RESTRICTED"):
		return "当前用户被限制卸载应用（多用户/儿童模式）"
	case strings.Contains(up, "NOT INSTALLED"):
		return "设备上未安装该包"
	case strings.Contains(up, "Failure"):
		return "adb 返回 Failure，详见输出"
	}
	return "adb 卸载失败，详见输出"
}

// isPackageInstalled 判断包是否存在于当前用户。
// Android 走 pm path，鸿蒙走 bm dump -n（未安装时返回 error）。
func isPackageInstalled(p device.Platform, connectKey, pkg string) bool {
	if p == device.PlatformHarmonyOS {
		return hdc.IsInstalled(connectKey, pkg)
	}
	res, err := adb.ExecuteAdbWithTimeout(
		[]string{"-s", connectKey, "shell", "pm", "path", pkg}, 10*time.Second)
	if err != nil || res == nil {
		return false
	}
	return strings.Contains(res.Output, "package:")
}

// hapInstallFailureHint 把 hdc install 的典型错误翻译成可执行的下一步。
func hapInstallFailureHint(output string) string {
	low := strings.ToLower(output)
	switch {
	case strings.Contains(low, "signature") || strings.Contains(low, "verify"):
		return "签名校验失败：HAP 的签名与设备上的已装版本不一致（换过调试证书），加 fresh=true 先卸载再装"
	case strings.Contains(low, "version code") || strings.Contains(low, "downgrade"):
		return "设备上已装版本更高，hdc 拒绝降级；确需降级请加 fresh=true"
	case strings.Contains(low, "not found") && !strings.Contains(low, "bundle"):
		return "安装包不存在或路径不可读：确认本机路径正确、且有读取权限"
	case strings.Contains(low, "no space") || strings.Contains(low, "storage"):
		return "设备存储空间不足，清理后再试"
	case strings.Contains(low, "device not support") || strings.Contains(low, "incompatible"):
		return "该 HAP 的目标设备类型/API 版本与当前设备不匹配"
	case strings.Contains(low, "install failed") || strings.Contains(low, "failed to install"):
		return "hdc 安装失败，详见下方输出（常见于签名、module.json 配置或 API 版本不匹配）"
	}
	return "hdc 安装失败，详见输出"
}

// hapUninstallFailureHint 把 hdc 卸载失败翻译成可执行的下一步。
func hapUninstallFailureHint(output string) string {
	low := strings.ToLower(output)
	switch {
	case strings.Contains(low, "not installed") || strings.Contains(low, "not found"):
		return "设备上未安装该包"
	case strings.Contains(low, "system") || strings.Contains(low, "preinstall"):
		return "系统预装应用不允许卸载；可在设备上手动停用"
	case strings.Contains(low, "permission"):
		return "权限不足：该包由系统签名保护，普通调试权限无法卸载"
	case strings.Contains(low, "fail"):
		return "hdc 返回失败，详见输出"
	}
	return "hdc 卸载失败，详见输出"
}

// tailText 截取文本末尾最多 max 字节（尽量落在行边界），避免把几 MB 的 adb 输出塞进工具结果。
func tailText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "…\n" + cut
}
