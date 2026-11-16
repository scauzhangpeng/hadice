package hdc

// 鸿蒙侧"运行态"查询：当前运行的进程集合与前台应用。
//
// 为什么不用 hdc.GetAppList：它内部并行跑 `bm dump -a -l` 与 `ps -A`，
// 但鸿蒙的 `ps -A` 只列出与 shell 同 uid 的进程，应用进程根本不在其中，
// 于是 IsRunning 恒为 false；`ps -ef` 才是全量视图（列尾为完整包名）。
// 同理，前台应用要看 `aa dump -l` 的 mission 列表，而不是 Android 的 dumpsys。

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// bundlePattern 匹配 aa dump 里的 "bundle name [com.xxx]" / "app name [com.xxx]"。
var bundlePattern = regexp.MustCompile(`(?:bundle|app) name \[([^\]]+)\]`)

// RunningPackages 返回当前有进程在跑的包名集合。
func RunningPackages(connectKey string) map[string]bool {
	out := make(map[string]bool)
	res, err := ExecuteHdcWithTimeout([]string{"-t", connectKey, "shell", "ps", "-ef"}, 10*time.Second)
	if err != nil || res == nil || !res.Success {
		return out
	}
	for _, line := range strings.Split(res.Output, "\n") {
		fields := strings.Fields(line)
		// 跳过表头（UID PID PPID ...）与空行；至少要有 uid/pid/cmd 三列。
		if len(fields) < 3 || fields[0] == "UID" {
			continue
		}
		// 列尾是命令行/进程名；应用进程这里就是完整 bundleName。
		name := fields[len(fields)-1]
		if strings.Contains(name, ".") {
			out[name] = true
		}
	}
	return out
}

// ForegroundApp 返回当前前台应用，格式与 Android 的 dumpsys 对齐：
//
//	com.huawei.hmos.settings/.MainAbility
//
// 也就是「包名/相对 ability 名」。调用方（如 MCP 的 launch_app）不必区分平台，
// 只取包名部分做前缀比较即可；取不到 ability 时退化为纯包名。
func ForegroundApp(connectKey string) string {
	res, err := ExecuteHdcWithTimeout([]string{"-t", connectKey, "shell", "aa", "dump", "-l"}, 15*time.Second)
	if err != nil || res == nil || !res.Success {
		return ""
	}
	bundle, ability := "", ""
	for _, line := range strings.Split(res.Output, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := bundlePattern.FindStringSubmatch(trimmed); len(m) == 2 {
			bundle = m[1]
		}
		if m := abilityPattern.FindStringSubmatch(trimmed); len(m) == 2 {
			ability = m[1]
		}
		// ability 的 state 与 app state 都会给出 #FOREGROUND；后台任务是 #BACKGROUND。
		if bundle != "" && strings.Contains(trimmed, "#FOREGROUND") {
			return joinBundleAbility(bundle, ability)
		}
	}
	return ""
}

// abilityPattern 匹配 aa dump 里的 "main name [com.xxx.MainAbility]"。
var abilityPattern = regexp.MustCompile(`main name \[([^\]]+)\]`)

// joinBundleAbility 把 "包名" + "包名.MainAbility" 拼成 Android 风格的
// "包名/.MainAbility"；无法归属到该包名时只返回包名。
func joinBundleAbility(bundle, ability string) string {
	if ability == "" || !strings.HasPrefix(ability, bundle+".") {
		return bundle
	}
	return bundle + "/." + strings.TrimPrefix(ability, bundle+".")
}

// IsInstalled 判断包是否安装在当前用户下。
func IsInstalled(connectKey, bundleName string) bool {
	res, err := ExecuteHdcWithTimeout(
		[]string{"-t", connectKey, "shell", "bm", "dump", "-n", bundleName}, 15*time.Second)
	if err != nil || res == nil || !res.Success {
		return false
	}
	out := res.Output
	if strings.Contains(strings.ToLower(out), "error") || strings.TrimSpace(out) == "" {
		return false
	}
	return strings.Contains(out, bundleName)
}

// PidOf 返回应用主进程的 pid（未运行返回 0）。
//
// 鸿蒙的 `pidof` 与 Android 语义一致，且比解析 `ps` 输出可靠：
// 进程名就是 bundleName，不存在截断问题。
func PidOf(connectKey, bundleName string) int {
	res, err := ExecuteHdcWithTimeout(
		[]string{"-t", connectKey, "shell", "pidof", bundleName}, 10*time.Second)
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

// AppVersion 返回已安装应用的版本名与版本号（未安装或解析失败返回空/0）。
func AppVersion(connectKey, bundleName string) (string, int) {
	res, err := ExecuteHdcWithTimeout(
		[]string{"-t", connectKey, "shell", "bm", "dump", "-n", bundleName}, 15*time.Second)
	if err != nil || res == nil || !res.Success {
		return "", 0
	}
	raw := res.Output
	i := strings.Index(raw, "{")
	if i < 0 {
		return "", 0
	}
	var d struct {
		VersionName string `json:"versionName"`
		VersionCode int    `json:"versionCode"`
	}
	if err := json.Unmarshal([]byte(raw[i:]), &d); err != nil {
		return "", 0
	}
	return d.VersionName, d.VersionCode
}
