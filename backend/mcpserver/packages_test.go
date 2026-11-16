package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Hadice/backend/device"
)

// TestNormalizePackagePath 校验路径前置检查：这些错误必须在触碰 adb/hdc 之前就被拦下，
// 否则模型会收到一个"安装失败"而不知道是自己路径写错了。
//
// 两个平台的包类型不同（Android .apk / 鸿蒙 .hap、.app），因此同一份校验
// 必须按平台给出不同结论——把 apk 装到鸿蒙、把 hap 装到安卓都要提前拦住。
func TestNormalizePackagePath(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	apk := write("app-debug.apk", []byte("PK\x03\x04fake"))
	upper := write("APP.APK", []byte("PK\x03\x04fake"))
	hap := write("entry-default-signed.hap", []byte("PK\x03\x04fake"))
	app := write("app-signed.app", []byte("PK\x03\x04fake"))
	txt := write("notes.txt", []byte("x"))
	apks := write("app.apks", []byte("PK\x03\x04fake"))

	cases := []struct {
		name     string
		platform device.Platform
		in       string
		wantErr  string
	}{
		{name: "安卓 正常 apk", platform: device.PlatformAndroid, in: apk},
		{name: "安卓 扩展名大小写不敏感", platform: device.PlatformAndroid, in: upper},
		{name: "安卓 空路径", platform: device.PlatformAndroid, in: "  ", wantErr: "apkPath 不能为空"},
		{name: "安卓 文件不存在", platform: device.PlatformAndroid, in: filepath.Join(dir, "nope.apk"), wantErr: "找不到安装包文件"},
		{name: "安卓 目录", platform: device.PlatformAndroid, in: dir, wantErr: "apkPath 指向目录"},
		{name: "安卓 非 apk 文件", platform: device.PlatformAndroid, in: txt, wantErr: "不支持的文件类型"},
		{name: "安卓 分包格式", platform: device.PlatformAndroid, in: apks, wantErr: "分包/打包格式"},
		{name: "安卓 收到 hap", platform: device.PlatformAndroid, in: hap, wantErr: "Android 设备需要 .apk"},

		{name: "鸿蒙 正常 hap", platform: device.PlatformHarmonyOS, in: hap},
		{name: "鸿蒙 正常 app", platform: device.PlatformHarmonyOS, in: app},
		{name: "鸿蒙 收到 apk", platform: device.PlatformHarmonyOS, in: apk, wantErr: "鸿蒙设备需要 .hap"},
		{name: "鸿蒙 非安装包", platform: device.PlatformHarmonyOS, in: txt, wantErr: "不支持的文件类型"},
		{name: "鸿蒙 文件不存在", platform: device.PlatformHarmonyOS, in: filepath.Join(dir, "nope.hap"), wantErr: "找不到安装包文件"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, size, err := normalizePackagePath(c.platform, c.in)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got path %q", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %q, want contains %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("expected absolute path, got %q", got)
			}
			if size <= 0 {
				t.Fatalf("expected positive size, got %d", size)
			}
		})
	}
}

// TestInstallFailureHint 安装失败要给出"下一步怎么做"，而不是原样抛 adb 输出。
func TestInstallFailureHint(t *testing.T) {
	cases := []struct{ output, want string }{
		{"Failure [INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected]", "fresh=true"},
		{"Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Package ... signatures do not match]", "签名"},
		{"Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]", "存储空间不足"},
		{"Failure [INSTALL_FAILED_USER_RESTRICTED: Install canceled by user]", "USB 安装"},
		{"Failure [INSTALL_FAILED_NO_MATCHING_ABIS: Failed to extract native libraries]", "CPU 架构"},
		{"Failure [INSTALL_FAILED_TEST_ONLY: installPackageLI]", "testOnly"},
		{"Failure [INSTALL_PARSE_FAILED_NO_CERTIFICATES]", "签名"},
		{"weird output", ""},
	}
	for _, c := range cases {
		got := installFailureHint(c.output)
		if c.want == "" {
			if got != "" {
				t.Fatalf("input %q: expected empty hint, got %q", c.output, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Fatalf("input %q: hint = %q, want contains %q", c.output, got, c.want)
		}
	}
}

// TestUninstallFailureHint 卸载失败同样要区分"系统包装不了/根本没装"等情形。
func TestUninstallFailureHint(t *testing.T) {
	if got := uninstallFailureHint("Failure [DELETE_FAILED_INTERNAL_ERROR]"); !strings.Contains(got, "系统预装") {
		t.Fatalf("unexpected hint: %q", got)
	}
	if got := uninstallFailureHint("Failure [not installed for 0]"); !strings.Contains(got, "未安装") {
		t.Fatalf("unexpected hint: %q", got)
	}
	if got := uninstallFailureHint("Success"); got == "" {
		t.Fatalf("expected non-empty fallback hint")
	}
}

// TestProtectedPackages 关键系统包必须被拒绝，避免一次误调用把设备搞废。
func TestProtectedPackages(t *testing.T) {
	for _, pkg := range []string{"android", "com.android.systemui", "com.android.settings", "com.android.phone"} {
		if _, ok := protectedPackages[pkg]; !ok {
			t.Fatalf("package %s should be protected", pkg)
		}
	}
}

// TestTailText 工具结果只保留尾部若干字节（adb 输出可能几 MB），且不切断行首。
func TestTailText(t *testing.T) {
	short := "Success"
	if got := tailText(short, 100); got != short {
		t.Fatalf("short text should be unchanged, got %q", got)
	}
	long := strings.Repeat("line of adb output\n", 200)
	got := tailText(long, 100)
	if len(got) > 110 { // 100 + "…\n"
		t.Fatalf("truncated text too long: %d", len(got))
	}
	if !strings.HasPrefix(got, "…\n") {
		t.Fatalf("truncated text should be marked, got %q", got)
	}
	// 除标记行外，其余必须是完整行——截断落在行中间会把日志行切坏。
	for i, line := range strings.Split(strings.TrimPrefix(got, "…\n"), "\n") {
		if line == "" || line == "line of adb output" {
			continue
		}
		t.Fatalf("line %d is a partial line: %q", i, line)
	}
}
