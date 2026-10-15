// node_exporter 是一个面向通用 Linux 主机的 Prometheus 采集器。
//
// 采集模块：
//
//	cpu         CPU 时间与使用率、系统负载、上下文切换、开机时间（默认开启）
//	mem         内存与 swap 使用量、换页次数、大页（默认开启）
//	hwmon       hwmon 与 thermal zone 温度（默认开启）
//	network     网卡流量、速率、链路状态，支持按网卡名筛选（默认开启）
//	systemd     systemd 服务状态，支持按 unit 名筛选（默认开启）
//	filesystem  文件系统容量/可用/使用率/只读状态（默认开启）
//	smart       硬盘 S.M.A.R.T.（默认关闭）
//
// 所有指标都会带上 instance=<hostname> 标签（见 instance 包）。
//
// 用法：node_exporter [flags]，监听地址默认为 :9100。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"node_exporter/cpu"
	"node_exporter/filesystem"
	"node_exporter/hwmon"
	"node_exporter/instance"
	"node_exporter/internal/util"
	"node_exporter/mem"
	"node_exporter/network"
	"node_exporter/smart"
	"node_exporter/systemd"
)

// buildVersion 与 buildCommit 在编译时通过 -ldflags 注入。
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

// version 由 main 在解析参数前填充，这里只为 -version 兜底。
var version = fmt.Sprintf("node_exporter %s", buildVersion)

// flags 保存全部命令行参数。
type flags struct {
	listenAddress  string
	telemetryPath  string
	authToken      string
	configPath     string
	procRoot       string
	sysRoot        string
	hostname       string
	commandTimeout time.Duration

	enableSystem     bool
	enableCPU        bool
	enableMem        bool
	enableTemp       bool
	enableSystemd    bool
	enableNetwork    bool
	enableSMART      bool
	enableFilesystem bool
	enableRuntime    bool

	systemdInclude string
	systemdExclude string
	systemdTypes   string
	systemdDetail  bool

	networkInclude string
	networkExclude string

	smartPath     string
	smartDevice   string
	smartExclude  string
	smartParallel int
	smartStandby  bool

	filesystemInclude       string
	filesystemExclude       string
	filesystemFstypeExclude string
	filesystemMountsPath    string
}

// fileConfig 对应 -config 指定的 JSON 配置文件。
//
// 所有字段都使用指针，以区分「配置里没写这个键」与「显式写了零值」：
// 前者保留命令行/默认值，后者才覆盖。键名统一使用 snake_case。
type fileConfig struct {
	ListenAddress  *string `json:"listen_address"`
	TelemetryPath  *string `json:"telemetry_path"`
	AuthToken      *string `json:"auth_token"`
	ProcRoot       *string `json:"proc_root"`
	SysRoot        *string `json:"sys_root"`
	Hostname       *string `json:"hostname"`
	CommandTimeout *string `json:"command_timeout"`

	EnableSystem     *bool `json:"enable_system"`
	EnableCPU        *bool `json:"enable_cpu"`
	EnableMem        *bool `json:"enable_mem"`
	EnableTemp       *bool `json:"enable_temp"`
	EnableSystemd    *bool `json:"enable_systemd"`
	EnableNetwork    *bool `json:"enable_network"`
	EnableSmart      *bool `json:"enable_smart"`
	EnableFilesystem *bool `json:"enable_filesystem"`
	EnableRuntime    *bool `json:"enable_runtime"`

	SystemdInclude *string `json:"systemd_include"`
	SystemdExclude *string `json:"systemd_exclude"`
	SystemdTypes   *string `json:"systemd_types"`
	SystemdDetail  *bool   `json:"systemd_detail"`

	NetworkInclude *string `json:"network_include"`
	NetworkExclude *string `json:"network_exclude"`

	SmartPath     *string `json:"smart_path"`
	SmartDevice   *string `json:"smart_device"`
	SmartExclude  *string `json:"smart_exclude"`
	SmartParallel *int    `json:"smart_parallel"`
	SmartStandby  *bool   `json:"smart_standby"`

	FilesystemInclude       *string `json:"filesystem_include"`
	FilesystemExclude       *string `json:"filesystem_exclude"`
	FilesystemFstypeExclude *string `json:"filesystem_fstype_exclude"`
	FilesystemMountsPath    *string `json:"filesystem_mounts_path"`
}

// registerFlags 定义全部命令行参数。
func registerFlags(fs *flag.FlagSet, f *flags) {
	fs.StringVar(&f.listenAddress, "web.listen-address", ":9100",
		"HTTP 监听地址，node_exporter 惯例为 :9100")
	fs.StringVar(&f.telemetryPath, "web.telemetry-path", "/metrics",
		"指标暴露路径")
	fs.StringVar(&f.authToken, "auth-token", "",
		"鉴权 token；非空时要求 Authorization: Bearer <token>")
	fs.StringVar(&f.configPath, "config", "",
		"JSON 配置文件路径；命令行显式给出的参数优先于配置文件")
	fs.StringVar(&f.procRoot, "proc.root", "/proc",
		"procfs 根目录（调试或容器场景可指向其他路径）")
	fs.StringVar(&f.sysRoot, "sys.root", "/sys",
		"sysfs 根目录")
	fs.StringVar(&f.hostname, "hostname", "",
		"覆盖自动识别的 hostname，留空则自动识别（用于 instance 标签）")
	fs.DurationVar(&f.commandTimeout, "command-timeout", 20*time.Second,
		"调用外部命令（systemctl/smartctl）的超时时间")

	fs.BoolVar(&f.enableSystem, "enable-system", true,
		"启用基础系统指标（下面几个开关在此为真时才生效）")
	fs.BoolVar(&f.enableCPU, "enable-cpu", true,
		"启用 CPU 与负载指标")
	fs.BoolVar(&f.enableMem, "enable-mem", true,
		"启用内存与 swap 指标")
	fs.BoolVar(&f.enableTemp, "enable-temp", true,
		"启用温度指标（hwmon 与 thermal zone）")

	fs.BoolVar(&f.enableSystemd, "enable-systemd", true,
		"启用 systemd 服务指标")
	fs.BoolVar(&f.enableNetwork, "enable-network", true,
		"启用网卡流量指标")
	fs.BoolVar(&f.enableFilesystem, "enable-filesystem", true,
		"启用文件系统容量指标（与 -enable-system 无关的独立开关）")

	fs.BoolVar(&f.enableSMART, "enable-smart", false,
		"启用硬盘 S.M.A.R.T. 指标（需要 smartmontools，通常需要 root）")
	fs.BoolVar(&f.enableRuntime, "enable-runtime", true,
		"启用 go_* 与 process_* 运行时指标")

	fs.StringVar(&f.systemdInclude, "systemd.include", "",
		"systemd unit 名包含正则：只导出匹配的 unit")
	fs.StringVar(&f.systemdExclude, "systemd.exclude", "",
		"systemd unit 名排除正则：导出时不包含匹配的 unit")
	fs.StringVar(&f.systemdTypes, "systemd.types", systemd.DefaultTypes,
		"要采集的 unit 类型，逗号分隔，例如 service,socket,mount")
	fs.BoolVar(&f.systemdDetail, "systemd.detail", true,
		"额外采集每个 unit 的内存/CPU 时间/任务数/重启次数（会调用 systemctl show，较慢）")

	fs.StringVar(&f.networkInclude, "network.include", "",
		"网卡名包含正则：只导出匹配的网卡")
	fs.StringVar(&f.networkExclude, "network.exclude", network.DefaultExcludeRegex,
		"网卡名排除正则；传空字符串可关闭默认的虚拟网卡排除")

	fs.StringVar(&f.smartPath, "smart.path", "smartctl", "smartctl 命令路径")
	fs.StringVar(&f.smartDevice, "smart.device", "",
		"要采集的硬盘列表，逗号分隔（如 /dev/sda,/dev/nvme0n1）；留空则用 smartctl --scan 自动发现")
	fs.StringVar(&f.smartExclude, "smart.exclude", smart.DefaultExcludeRegex,
		"硬盘路径排除正则；传空字符串可采集 smartctl 扫描到的所有设备")
	fs.IntVar(&f.smartParallel, "smart.parallel", 4,
		"并发执行 smartctl 的最大数量")
	fs.BoolVar(&f.smartStandby, "smart.standby", true,
		"硬盘处于待机时不唤醒它（等价于 smartctl -n standby）")

	fs.StringVar(&f.filesystemInclude, "filesystem.include", "",
		"挂载点包含正则：只导出匹配的挂载点")
	fs.StringVar(&f.filesystemExclude, "filesystem.exclude", "",
		"挂载点排除正则：导出时不包含匹配的挂载点")
	fs.StringVar(&f.filesystemFstypeExclude, "filesystem.fstype-exclude",
		filesystem.DefaultFstypeExclude,
		"文件系统类型排除正则；传空字符串可采集所有类型")
	fs.StringVar(&f.filesystemMountsPath, "filesystem.mounts-path", "/proc/mounts",
		"挂载表路径（配合 -proc.root，便于测试注入）")
}

// loadConfig 读取并解析 JSON 配置文件，未知键会报错。
func loadConfig(path string) (*fileConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法打开配置文件 %s: %w", path, err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()

	var cfg fileConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return &cfg, nil
}

// mergeConfig 把配置文件里出现、且命令行没有显式设置的项合并进 flags。
//
// 判定「命令行是否显式设置」一律使用 flag.Visit 收集到的 flag 名，
// 绝不使用「值 == 默认值」这种不可靠的判断。
func mergeConfig(fs *flag.FlagSet, f *flags, cfg *fileConfig) error {
	visited := make(map[string]bool)
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	setStr := func(name string, dst *string, src *string) {
		if src != nil && !visited[name] {
			*dst = *src
		}
	}
	setBool := func(name string, dst *bool, src *bool) {
		if src != nil && !visited[name] {
			*dst = *src
		}
	}
	setInt := func(name string, dst *int, src *int) {
		if src != nil && !visited[name] {
			*dst = *src
		}
	}

	setStr("web.listen-address", &f.listenAddress, cfg.ListenAddress)
	setStr("web.telemetry-path", &f.telemetryPath, cfg.TelemetryPath)
	setStr("auth-token", &f.authToken, cfg.AuthToken)
	setStr("proc.root", &f.procRoot, cfg.ProcRoot)
	setStr("sys.root", &f.sysRoot, cfg.SysRoot)
	setStr("hostname", &f.hostname, cfg.Hostname)

	if cfg.CommandTimeout != nil && !visited["command-timeout"] {
		d, err := time.ParseDuration(*cfg.CommandTimeout)
		if err != nil {
			return fmt.Errorf("配置文件中的 command_timeout 非法: %w", err)
		}
		f.commandTimeout = d
	}

	setBool("enable-system", &f.enableSystem, cfg.EnableSystem)
	setBool("enable-cpu", &f.enableCPU, cfg.EnableCPU)
	setBool("enable-mem", &f.enableMem, cfg.EnableMem)
	setBool("enable-temp", &f.enableTemp, cfg.EnableTemp)
	setBool("enable-systemd", &f.enableSystemd, cfg.EnableSystemd)
	setBool("enable-network", &f.enableNetwork, cfg.EnableNetwork)
	setBool("enable-smart", &f.enableSMART, cfg.EnableSmart)
	setBool("enable-filesystem", &f.enableFilesystem, cfg.EnableFilesystem)
	setBool("enable-runtime", &f.enableRuntime, cfg.EnableRuntime)

	setStr("systemd.include", &f.systemdInclude, cfg.SystemdInclude)
	setStr("systemd.exclude", &f.systemdExclude, cfg.SystemdExclude)
	setStr("systemd.types", &f.systemdTypes, cfg.SystemdTypes)
	setBool("systemd.detail", &f.systemdDetail, cfg.SystemdDetail)

	setStr("network.include", &f.networkInclude, cfg.NetworkInclude)
	setStr("network.exclude", &f.networkExclude, cfg.NetworkExclude)

	setStr("smart.path", &f.smartPath, cfg.SmartPath)
	setStr("smart.device", &f.smartDevice, cfg.SmartDevice)
	setStr("smart.exclude", &f.smartExclude, cfg.SmartExclude)
	setInt("smart.parallel", &f.smartParallel, cfg.SmartParallel)
	setBool("smart.standby", &f.smartStandby, cfg.SmartStandby)

	setStr("filesystem.include", &f.filesystemInclude, cfg.FilesystemInclude)
	setStr("filesystem.exclude", &f.filesystemExclude, cfg.FilesystemExclude)
	setStr("filesystem.fstype-exclude", &f.filesystemFstypeExclude, cfg.FilesystemFstypeExclude)
	setStr("filesystem.mounts-path", &f.filesystemMountsPath, cfg.FilesystemMountsPath)

	return nil
}

// validateListenAddress 校验监听地址：必须是 host:port 且端口在 1-65535。
func validateListenAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("监听地址 %q 非法: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("监听地址 %q 的端口 %q 非法（应在 1-65535 之间）", addr, port)
	}
	return nil
}

// maskToken 对鉴权 token 打码：只保留前 4 位，空 token 显示为 (未启用)。
func maskToken(token string) string {
	if token == "" {
		return "(未启用)"
	}
	n := 4
	if len(token) < n {
		n = len(token)
	}
	return token[:n] + "***"
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[node_exporter] ")

	var (
		f           flags
		showVersion bool
	)
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	registerFlags(fs, &f)
	fs.BoolVar(&showVersion, "version", false, "打印版本信息后退出")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "node_exporter —— 主机指标采集器（默认监听 :9100）\n\n用法: %s [flags]\n\n", fs.Name())
		fs.PrintDefaults()
	}
	version = versionString()
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatalf("解析命令行参数失败: %v", err)
	}
	if showVersion {
		fmt.Println(version)
		return
	}

	// ---- 配置文件合并（必须发生在任何派生变量计算之前）----
	if f.configPath != "" {
		cfg, err := loadConfig(f.configPath)
		if err != nil {
			log.Fatalf("配置非法: %v", err)
		}
		if err := mergeConfig(fs, &f, cfg); err != nil {
			log.Fatalf("配置非法: %v", err)
		}
	}

	// ---- 校验监听地址与超时 ----
	if err := validateListenAddress(f.listenAddress); err != nil {
		log.Fatalf("配置非法: %v", err)
	}
	if f.commandTimeout <= 0 {
		log.Fatalf("配置非法: command-timeout 必须大于 0，当前为 %v", f.commandTimeout)
	}
	if !strings.HasPrefix(f.telemetryPath, "/") {
		log.Fatalf("配置非法: web.telemetry-path 必须以 / 开头，当前为 %q", f.telemetryPath)
	}

	// ---- hostname 与 instance 标签 ----
	host := instance.Detect(f.hostname)
	if host == "unknown" {
		log.Printf("警告: 未能识别 hostname，instance 标签将使用 %q，"+
			"可通过 -hostname 显式指定", host)
	}

	// ---- 筛选规则 ----
	systemdFilter, err := util.NewFilter(f.systemdInclude, f.systemdExclude)
	if err != nil {
		log.Fatalf("systemd 筛选规则无效: %v", err)
	}
	networkFilter, err := util.NewFilter(f.networkInclude, f.networkExclude)
	if err != nil {
		log.Fatalf("网卡筛选规则无效: %v", err)
	}
	smartFilter, err := util.NewFilter("", f.smartExclude)
	if err != nil {
		log.Fatalf("硬盘筛选规则无效: %v", err)
	}
	filesystemFilter, err := util.NewFilter(f.filesystemInclude, f.filesystemExclude)
	if err != nil {
		log.Fatalf("文件系统挂载点筛选规则无效: %v", err)
	}
	filesystemFstypeFilter, err := util.NewFilter("", f.filesystemFstypeExclude)
	if err != nil {
		log.Fatalf("文件系统类型筛选规则无效: %v", err)
	}

	// 挂载表默认跟随 -proc.root（例如 -proc.root=/tmp/x -> /tmp/x/mounts），
	// 但命令行显式给出 -filesystem.mounts-path 时以显式值为准。
	mountsPath := f.filesystemMountsPath
	explicitMounts := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "filesystem.mounts-path" {
			explicitMounts = true
		}
	})
	if !explicitMounts {
		mountsPath = filepath.Join(f.procRoot, "mounts")
	}

	// ---- 注册采集器 ----
	registry := prometheus.NewRegistry()
	var enabled []string

	enableCPU := f.enableSystem && f.enableCPU
	enableMem := f.enableSystem && f.enableMem
	enableTemp := f.enableSystem && f.enableTemp

	if enableCPU {
		registry.MustRegister(cpu.New(f.procRoot))
		enabled = append(enabled, "cpu")
	}
	if enableMem {
		registry.MustRegister(mem.New(f.procRoot))
		enabled = append(enabled, "mem")
	}
	if enableTemp {
		registry.MustRegister(hwmon.New(f.sysRoot))
		enabled = append(enabled, "temperature")
	}
	if f.enableNetwork {
		registry.MustRegister(network.New(f.procRoot, f.sysRoot, networkFilter))
		enabled = append(enabled, "network")
	}
	if f.enableSystemd {
		systemdExporter := systemd.New(systemd.Options{
			Bin:        "systemctl",
			Filter:     systemdFilter,
			Types:      util.SplitList(f.systemdTypes),
			WithDetail: f.systemdDetail,
			Timeout:    f.commandTimeout,
		})
		registry.MustRegister(systemdExporter)
		enabled = append(enabled, "systemd")
		log.Printf("systemd 采集: %s", systemdExporter)
	}
	if f.enableFilesystem {
		registry.MustRegister(filesystem.New(filesystem.Options{
			MountsPath:   mountsPath,
			Filter:       filesystemFilter,
			FstypeFilter: filesystemFstypeFilter,
		}))
		enabled = append(enabled, "filesystem")
	}
	if f.enableSMART {
		if err := util.LookPath(f.smartPath); err != nil {
			log.Printf("警告: 找不到 %s，S.M.A.R.T. 指标将为空（%v）", f.smartPath, err)
		}
		smartExporter := smart.New(smart.Options{
			Bin:       f.smartPath,
			Devices:   util.SplitList(f.smartDevice),
			Filter:    smartFilter,
			NoStandby: f.smartStandby,
			Parallel:  f.smartParallel,
			Timeout:   f.commandTimeout,
		})
		registry.MustRegister(smartExporter)
		enabled = append(enabled, "smart")
		log.Printf("S.M.A.R.T. 采集: %s", smartExporter)
	}
	if f.enableRuntime {
		// go_* 反映 exporter 自身状态，process_* 反映 exporter 进程资源占用。
		registry.MustRegister(collectors.NewGoCollector())
		registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		enabled = append(enabled, "runtime")
	}

	if len(enabled) == 0 {
		log.Fatalf("未启用任何采集模块，请至少开启一个（见 -h）")
	}

	// ---- 用 instance=<hostname> 改写所有指标的 instance 标签 ----
	gatherer := instance.WrapGatherer(registry, host)
	handler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		// 单个采集器失败不应该让整次抓取返回 500：
		// 各采集器内部已用 *_scrape_error 指标表达错误，这里兜底。
		ErrorHandling: promhttp.ContinueOnError,
	})

	mux := http.NewServeMux()
	mux.Handle(f.telemetryPath, handler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html>
<head><title>node_exporter</title></head>
<body>
<h1>node_exporter</h1>
<p>instance: <code>%s</code></p>
<p>版本: <code>%s</code></p>
<p>已启用模块: <code>%s</code></p>
<p><a href="%s">Metrics</a></p>
</body>
</html>`, host, version, strings.Join(enabled, ", "), f.telemetryPath)
	})

	// ---- 鉴权（/favicon.ico 在鉴权前短路返回 204）----
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f.authToken != "" {
			if !hasValidToken(r, f.authToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              f.listenAddress,
		Handler:           authHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 写超时必须大于单次采集耗时（硬盘较多时 smartctl 并发采集可能偏慢），
		// 因此给足余量。
		WriteTimeout: 3 * f.commandTimeout,
		IdleTimeout:  60 * time.Second,
	}

	// ---- 启动日志（打印实际生效的完整配置）----
	log.Printf("版本: %s (%s %s)", buildVersion, runtime.Version(), runtime.GOARCH)
	log.Printf("监听地址: %s", f.listenAddress)
	log.Printf("指标路径: %s", f.telemetryPath)
	log.Printf("鉴权 token: %s", maskToken(f.authToken))
	if f.authToken == "" {
		log.Printf("警告: 未设置 auth_token，/metrics 将不做鉴权")
	}
	log.Printf("instance 标签: %s", host)
	log.Printf("proc.root=%s sys.root=%s filesystem.mounts-path=%s", f.procRoot, f.sysRoot, mountsPath)
	log.Printf("command-timeout: %s", f.commandTimeout)
	log.Printf("已启用模块: %s", strings.Join(enabled, ", "))
	if f.enableNetwork {
		log.Printf("网卡筛选: %s", networkFilter)
	}
	if f.enableSystemd {
		log.Printf("systemd 筛选: %s（types=%s detail=%v）",
			systemdFilter, f.systemdTypes, f.systemdDetail)
	}
	if f.enableFilesystem {
		log.Printf("文件系统筛选: 挂载点(%s) 类型排除(%s)",
			filesystemFilter, filesystemFstypeFilter)
	}
	if f.enableSMART {
		log.Printf("SMART 筛选: %s（device=%q parallel=%d standby=%v）",
			smartFilter, f.smartDevice, f.smartParallel, f.smartStandby)
	}
	log.Printf("开始监听 %s%s", f.listenAddress, f.telemetryPath)

	// ---- 优雅退出 ----
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("HTTP 服务异常退出: %v", err)
	case <-ctx.Done():
		log.Printf("收到退出信号，正在关闭 HTTP 服务…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭 HTTP 服务时出错: %v", err)
	}
	log.Printf("已退出")
}

// hasValidToken 校验请求头中的 Bearer token。
func hasValidToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix)) == token
}

// versionString 拼装版本信息，包含 VCS 信息（若编译时可用）。
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fmt.Sprintf("node_exporter %s (commit %s, built %s)",
			buildVersion, buildCommit, buildDate)
	}

	var rev, dirty, date string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value
		case "vcs.time":
			date = s.Value
		}
	}
	// 优先使用 ldflags 注入的值，其次使用 VCS 信息。
	if buildCommit != "unknown" {
		rev = buildCommit
	}
	if buildDate != "unknown" {
		date = buildDate
	}
	if rev != "" && len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty == "true" {
		rev += "-dirty"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "node_exporter %s", buildVersion)
	fmt.Fprintf(&b, " (%s %s/%s", info.GoVersion, runtime.GOOS, runtime.GOARCH)
	if rev != "" && rev != "unknown" {
		fmt.Fprintf(&b, ", commit %s", rev)
	}
	if date != "" && date != "unknown" {
		fmt.Fprintf(&b, ", built %s", date)
	}
	b.WriteString(")")
	return b.String()
}
