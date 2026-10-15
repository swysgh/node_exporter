package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"node_exporter/filesystem"
	"node_exporter/internal/util"
	"node_exporter/network"
	"node_exporter/smart"
	"node_exporter/systemd"
)

// newRequestWithAuth 构造一个带（或不带）Authorization 头的测试请求。
func newRequestWithAuth(auth string) (*http.Request, error) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req, nil
}

// parseFlags 用给定参数解析出一份配置。
func parseFlags(t *testing.T, args ...string) flags {
	t.Helper()
	var f flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	registerFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数 %v 失败: %v", args, err)
	}
	return f
}

// parseFlagsWithConfig 解析参数并应用 -config 指向的配置文件，
// 与 main 的处理顺序一致（Parse -> loadConfig -> mergeConfig）。
func parseFlagsWithConfig(t *testing.T, args ...string) (flags, error) {
	t.Helper()
	var f flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	registerFlags(fs, &f)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数 %v 失败: %v", args, err)
	}
	if f.configPath != "" {
		cfg, err := loadConfig(f.configPath)
		if err != nil {
			return f, err
		}
		if err := mergeConfig(fs, &f, cfg); err != nil {
			return f, err
		}
	}
	return f, nil
}

// writeConfig 把内容写入临时文件并返回路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

// TestDefaultFlags 锁定需求里约定的默认行为。
func TestDefaultFlags(t *testing.T) {
	f := parseFlags(t)

	if f.listenAddress != ":9100" {
		t.Errorf("默认监听地址 = %q，期望 :9100", f.listenAddress)
	}
	if f.telemetryPath != "/metrics" {
		t.Errorf("默认指标路径 = %q，期望 /metrics", f.telemetryPath)
	}
	if f.authToken != "" {
		t.Errorf("默认 auth-token = %q，期望空", f.authToken)
	}
	if f.configPath != "" {
		t.Errorf("默认 config = %q，期望空", f.configPath)
	}

	// 默认关闭的项。
	if f.enableSMART {
		t.Error("S.M.A.R.T. 默认应为关闭")
	}

	// 默认开启的项。
	if !f.enableSystem || !f.enableCPU || !f.enableMem || !f.enableTemp {
		t.Errorf("CPU/内存/温度默认应为开启: system=%v cpu=%v mem=%v temp=%v",
			f.enableSystem, f.enableCPU, f.enableMem, f.enableTemp)
	}
	if !f.enableSystemd {
		t.Error("systemd 默认应为开启")
	}
	if !f.enableNetwork {
		t.Error("网卡流量默认应为开启")
	}
	if !f.enableFilesystem {
		t.Error("文件系统默认应为开启")
	}

	// hostname 默认为空，表示自动识别。
	if f.hostname != "" {
		t.Errorf("默认 hostname = %q，期望空（自动识别）", f.hostname)
	}

	// 筛选规则的默认值。
	if f.systemdInclude != "" || f.systemdExclude != "" {
		t.Errorf("systemd 默认不应有筛选: include=%q exclude=%q", f.systemdInclude, f.systemdExclude)
	}
	if f.networkInclude != "" {
		t.Errorf("network.include 默认 = %q，期望空", f.networkInclude)
	}
	if f.networkExclude != network.DefaultExcludeRegex {
		t.Errorf("network.exclude 默认 = %q，期望使用内置的虚拟网卡排除规则", f.networkExclude)
	}
	if f.systemdTypes != systemd.DefaultTypes {
		t.Errorf("systemd.types 默认 = %q，期望 %q", f.systemdTypes, systemd.DefaultTypes)
	}
	if !f.systemdDetail {
		t.Error("systemd.detail 默认应为开启")
	}

	// SMART 的默认值。
	if f.smartExclude != smart.DefaultExcludeRegex {
		t.Errorf("smart.exclude 默认 = %q", f.smartExclude)
	}
	if !f.smartStandby {
		t.Error("smart.standby 默认应为开启（不唤醒待机盘）")
	}
	if f.smartParallel != 4 {
		t.Errorf("smart.parallel 默认 = %d，期望 4", f.smartParallel)
	}

	// 文件系统的默认值。
	if f.filesystemInclude != "" || f.filesystemExclude != "" {
		t.Errorf("filesystem 默认不应有挂载点筛选: include=%q exclude=%q",
			f.filesystemInclude, f.filesystemExclude)
	}
	if f.filesystemFstypeExclude != filesystem.DefaultFstypeExclude {
		t.Errorf("filesystem.fstype-exclude 默认 = %q", f.filesystemFstypeExclude)
	}
	if f.filesystemMountsPath != "/proc/mounts" {
		t.Errorf("filesystem.mounts-path 默认 = %q", f.filesystemMountsPath)
	}

	if f.commandTimeout != 20*time.Second {
		t.Errorf("command-timeout 默认 = %v，期望 20s", f.commandTimeout)
	}
}

// TestOverrideFlags 验证命令行可以覆盖默认值。
func TestOverrideFlags(t *testing.T) {
	f := parseFlags(t,
		"-web.listen-address=:19100",
		"-hostname=web01.example.com",
		"-enable-smart",
		"-enable-system=false",
		"-enable-filesystem=false",
		"-network.include=^eth",
		"-network.exclude=",
		"-systemd.include=^(ssh|nginx)",
		"-systemd.types=service,socket",
		"-smart.device=/dev/sda,/dev/sdb",
		"-smart.parallel=8",
		"-filesystem.include=^/data",
		"-filesystem.fstype-exclude=",
		"-command-timeout=5s",
	)

	if f.listenAddress != ":19100" {
		t.Errorf("listenAddress = %q", f.listenAddress)
	}
	if f.hostname != "web01.example.com" {
		t.Errorf("hostname = %q", f.hostname)
	}
	if !f.enableSMART {
		t.Error("命令行无法开启 smart")
	}
	if f.enableSystem {
		t.Error("命令行无法关闭 system")
	}
	if f.enableFilesystem {
		t.Error("命令行无法关闭 filesystem")
	}
	if f.networkInclude != "^eth" {
		t.Errorf("network.include = %q", f.networkInclude)
	}
	// 传空字符串应当能关闭默认排除规则。
	if f.networkExclude != "" {
		t.Errorf("network.exclude = %q，期望空字符串", f.networkExclude)
	}
	if f.systemdInclude != "^(ssh|nginx)" {
		t.Errorf("systemd.include = %q", f.systemdInclude)
	}
	if f.systemdTypes != "service,socket" {
		t.Errorf("systemd.types = %q", f.systemdTypes)
	}
	if f.smartDevice != "/dev/sda,/dev/sdb" {
		t.Errorf("smart.device = %q", f.smartDevice)
	}
	if f.smartParallel != 8 {
		t.Errorf("smart.parallel = %d", f.smartParallel)
	}
	if f.filesystemInclude != "^/data" {
		t.Errorf("filesystem.include = %q", f.filesystemInclude)
	}
	if f.filesystemFstypeExclude != "" {
		t.Errorf("filesystem.fstype-exclude = %q，期望空字符串", f.filesystemFstypeExclude)
	}
	if f.commandTimeout != 5*time.Second {
		t.Errorf("command-timeout = %v", f.commandTimeout)
	}
}

// TestConfigApplied 验证配置文件能设置命令行未显式给出的项。
func TestConfigApplied(t *testing.T) {
	path := writeConfig(t, `{
		"listen_address": ":19999",
		"auth_token": "s3cr3t",
		"enable_smart": true,
		"enable_filesystem": false,
		"command_timeout": "7s",
		"filesystem_include": "^/data",
		"smart_parallel": 9
	}`)

	f, err := parseFlagsWithConfig(t, "-config="+path)
	if err != nil {
		t.Fatalf("合并配置失败: %v", err)
	}
	if f.listenAddress != ":19999" {
		t.Errorf("listenAddress = %q，期望 :19999", f.listenAddress)
	}
	if f.authToken != "s3cr3t" {
		t.Errorf("authToken = %q", f.authToken)
	}
	if !f.enableSMART {
		t.Error("配置文件未能开启 smart")
	}
	if f.enableFilesystem {
		t.Error("配置文件未能关闭 filesystem")
	}
	if f.commandTimeout != 7*time.Second {
		t.Errorf("commandTimeout = %v，期望 7s", f.commandTimeout)
	}
	if f.filesystemInclude != "^/data" {
		t.Errorf("filesystemInclude = %q", f.filesystemInclude)
	}
	if f.smartParallel != 9 {
		t.Errorf("smartParallel = %d", f.smartParallel)
	}
	// 未在配置文件里出现的项应保留默认值。
	if f.telemetryPath != "/metrics" {
		t.Errorf("telemetryPath = %q，期望保留默认 /metrics", f.telemetryPath)
	}
}

// TestFlagOverridesConfig 验证命令行显式给出的项优先于配置文件。
func TestFlagOverridesConfig(t *testing.T) {
	path := writeConfig(t, `{"listen_address": ":19999", "enable_filesystem": false}`)

	f, err := parseFlagsWithConfig(t, "-config="+path,
		"-web.listen-address=:12345", "-enable-filesystem=true")
	if err != nil {
		t.Fatalf("合并配置失败: %v", err)
	}
	if f.listenAddress != ":12345" {
		t.Errorf("listenAddress = %q，期望命令行值 :12345", f.listenAddress)
	}
	if !f.enableFilesystem {
		t.Error("命令行显式开启 filesystem 应覆盖配置文件")
	}
}

// TestConfigExplicitZeroValue 验证配置文件里显式写的零值（false）能生效，
// 不会被误判为「没设置」。
func TestConfigExplicitZeroValue(t *testing.T) {
	path := writeConfig(t, `{"enable_system": false}`)
	f, err := parseFlagsWithConfig(t, "-config="+path)
	if err != nil {
		t.Fatalf("合并配置失败: %v", err)
	}
	if f.enableSystem {
		t.Error("配置文件里 enable_system=false 应当生效")
	}
}

// TestConfigUnknownKey 验证拼错的键会报错而不是被静默忽略。
func TestConfigUnknownKey(t *testing.T) {
	path := writeConfig(t, `{"nope": 1}`)
	if _, err := parseFlagsWithConfig(t, "-config="+path); err == nil {
		t.Error("未知键应当导致解析失败")
	}
}

// TestConfigInvalidDuration 验证非法的 command_timeout 会报错。
func TestConfigInvalidDuration(t *testing.T) {
	path := writeConfig(t, `{"command_timeout": "abc"}`)
	if _, err := parseFlagsWithConfig(t, "-config="+path); err == nil {
		t.Error("非法 command_timeout 应当导致解析失败")
	}
}

// TestConfigMissingFile 验证配置文件不存在时报错。
func TestConfigMissingFile(t *testing.T) {
	if _, err := parseFlagsWithConfig(t, "-config=/nonexistent/config.json"); err == nil {
		t.Error("配置文件不存在时应当报错")
	}
}

func TestValidateListenAddress(t *testing.T) {
	valid := []string{":9100", "127.0.0.1:19100", "[::1]:9100", "0.0.0.0:65535"}
	for _, a := range valid {
		if err := validateListenAddress(a); err != nil {
			t.Errorf("validateListenAddress(%q) = %v，期望合法", a, err)
		}
	}
	invalid := []string{"bad", "127.0.0.1", ":0", ":70000", "host:port", ""}
	for _, a := range invalid {
		if err := validateListenAddress(a); err == nil {
			t.Errorf("validateListenAddress(%q) 期望报错", a)
		}
	}
}

func TestMaskToken(t *testing.T) {
	cases := map[string]string{
		"":             "(未启用)",
		"abcdef123456": "abcd***",
		"ab":           "ab***",
	}
	for in, want := range cases {
		if got := maskToken(in); got != want {
			t.Errorf("maskToken(%q) = %q，期望 %q", in, got, want)
		}
	}
	if strings.Contains(maskToken("abcdef123456"), "ef123456") {
		t.Error("打码后不应包含 token 的后半部分")
	}
}

func TestHasValidToken(t *testing.T) {
	req, _ := newRequestWithAuth("Bearer secret")
	if !hasValidToken(req, "secret") {
		t.Error("正确的 Bearer token 应当通过")
	}
	req, _ = newRequestWithAuth("Bearer wrong")
	if hasValidToken(req, "secret") {
		t.Error("错误的 token 不应通过")
	}
	req, _ = newRequestWithAuth("")
	if hasValidToken(req, "secret") {
		t.Error("缺少 Authorization 头不应通过")
	}
}

// TestInvalidRegexRejected 验证非法正则会在启动时报错而不是静默失效。
func TestInvalidRegexRejected(t *testing.T) {
	if _, err := util.NewFilter("[bad", ""); err == nil {
		t.Error("非法 include 正则应当返回错误")
	}
	if _, err := util.NewFilter("", "[bad"); err == nil {
		t.Error("非法 exclude 正则应当返回错误")
	}
}

func TestVersionString(t *testing.T) {
	v := versionString()
	if !strings.Contains(v, "node_exporter") {
		t.Errorf("版本串 = %q，应当包含 node_exporter", v)
	}
	if !strings.Contains(v, "go1.") {
		t.Errorf("版本串 = %q，应当包含 Go 版本", v)
	}
}

func TestHelpMentionsAllFilters(t *testing.T) {
	var f flags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var sb strings.Builder
	fs.SetOutput(&sb)
	registerFlags(fs, &f)
	fs.PrintDefaults()

	usage := sb.String()
	for _, want := range []string{
		"systemd.include", "systemd.exclude", "systemd.types",
		"network.include", "network.exclude",
		"enable-smart", "enable-filesystem", "web.listen-address",
		"auth-token", "config",
		"filesystem.include", "filesystem.exclude",
		"filesystem.fstype-exclude", "filesystem.mounts-path",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("帮助信息缺少 %s", want)
		}
	}
}
