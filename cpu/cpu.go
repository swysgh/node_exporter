// Package cpu 采集 CPU 时间、负载、上下文切换、开机时间等基础指标，
// 数据全部来自 /proc 文件系统，不依赖任何外部命令。
package cpu

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// UserHz 是内核的用户时钟频率（sysconf(_SC_CLK_TCK)）。
// Linux 上除个别古老架构外一律为 100，CPU 时间从 jiffies 换算成秒时使用。
const UserHz = 100.0

// cpuModes 是 /proc/stat 中 CPU 时间字段的顺序与对应标签值。
// 注意：guest 时间已经包含在 user 时间里，累加总时间时不能重复计算，
// 但仍然原样导出，便于使用者自行判断。
var cpuModes = []string{
	"user", "nice", "system", "idle", "iowait",
	"irq", "softirq", "steal", "guest", "guest_nice",
}

// CPUStats 保存单个 CPU 的时间统计（单位 jiffies）。
type CPUStats struct {
	User      uint64
	Nice      uint64
	System    uint64
	Idle      uint64
	Iowait    uint64
	IRQ       uint64
	SoftIRQ   uint64
	Steal     uint64
	Guest     uint64
	GuestNice uint64
}

// Total 返回除 guest/guest_nice 之外的时间之和。
//
// guest 时间是 user 时间的子集，重复累加会让总时间大于真实值，
// 导致使用率计算失真，因此这里不计入。
func (c CPUStats) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.Iowait + c.IRQ + c.SoftIRQ + c.Steal
}

// IdleTotal 返回空闲时间，含 IO 等待。
func (c CPUStats) IdleTotal() uint64 { return c.Idle + c.Iowait }

// BusyTotal 返回非空闲时间。
func (c CPUStats) BusyTotal() uint64 { return c.Total() - c.IdleTotal() }

// value 按 cpuModes 顺序返回第 i 个时间字段。
func (c CPUStats) value(i int) uint64 {
	switch i {
	case 0:
		return c.User
	case 1:
		return c.Nice
	case 2:
		return c.System
	case 3:
		return c.Idle
	case 4:
		return c.Iowait
	case 5:
		return c.IRQ
	case 6:
		return c.SoftIRQ
	case 7:
		return c.Steal
	case 8:
		return c.Guest
	case 9:
		return c.GuestNice
	}
	return 0
}

// ProcStat 保存 /proc/stat 中除 CPU 时间之外的其他统计量。
type ProcStat struct {
	CPUs         map[string]CPUStats // "total" 与 "0"/"1"/... 形式的每核时间
	ContextSw    uint64              // ctxt：上下文切换次数
	Forks        uint64              // processes：创建的进程数
	BootTime     uint64              // btime：开机时间戳（Unix 秒）
	ProcsRunning uint64
	ProcsBlocked uint64
}

// ParseProcStat 解析 /proc/stat 的内容。
//
// 输入形如：
//
//	cpu  1234 56 789 90123 45 6 7 0 0 0
//	cpu0 617 28 395 45061 22 3 3 0 0 0
//	...
//	ctxt 12345678
//	btime 1700000000
//	processes 98765
//	procs_running 2
//	procs_blocked 0
func ParseProcStat(content string) ProcStat {
	ps := ProcStat{CPUs: make(map[string]CPUStats)}

	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "cpu":
			ps.CPUs["total"] = parseCPUFields(fields)
		case "ctxt":
			ps.ContextSw = util.ParseUint64(fieldAt(fields, 1))
		case "btime":
			ps.BootTime = util.ParseUint64(fieldAt(fields, 1))
		case "processes":
			ps.Forks = util.ParseUint64(fieldAt(fields, 1))
		case "procs_running":
			ps.ProcsRunning = util.ParseUint64(fieldAt(fields, 1))
		case "procs_blocked":
			ps.ProcsBlocked = util.ParseUint64(fieldAt(fields, 1))
		default:
			// cpu0、cpu1 ... 这类每核行。
			if strings.HasPrefix(fields[0], "cpu") {
				if id, err := strconv.Atoi(fields[0][3:]); err == nil {
					ps.CPUs[fields[0]] = parseCPUFields(fields)
					_ = id
				}
			}
		}
	}
	return ps
}

// fieldAt 安全地取 fields 的第 i 个字段，越界返回 ""。
func fieldAt(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// parseCPUFields 把 /proc/stat 中一行 CPU 字段转成 CPUStats。
func parseCPUFields(fields []string) CPUStats {
	cs := CPUStats{}
	targets := []*uint64{
		&cs.User, &cs.Nice, &cs.System, &cs.Idle, &cs.Iowait,
		&cs.IRQ, &cs.SoftIRQ, &cs.Steal, &cs.Guest, &cs.GuestNice,
	}
	for i, dst := range targets {
		*dst = util.ParseUint64(fieldAt(fields, i+1))
	}
	return cs
}

// LoadAvg 保存 /proc/loadavg 的解析结果。
type LoadAvg struct {
	Load1        float64
	Load5        float64
	Load15       float64
	RunningTotal uint64 // "1/234" 中的分母：可运行线程总数
}

// ParseLoadAvg 解析形如 "0.52 0.58 0.59 2/1234 56789" 的一行。
func ParseLoadAvg(content string) (LoadAvg, error) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return LoadAvg{}, fmt.Errorf("unexpected loadavg format: %q", strings.TrimSpace(content))
	}
	la := LoadAvg{
		Load1:  util.ParseFloat64(fields[0]),
		Load5:  util.ParseFloat64(fields[1]),
		Load15: util.ParseFloat64(fields[2]),
	}
	// 第 4 个字段形如 "2/1234"，分母即线程总数。
	if len(fields) > 3 {
		if _, total, ok := strings.Cut(fields[3], "/"); ok {
			la.RunningTotal = util.ParseUint64(total)
		}
	}
	return la, nil
}

// CPUInfo 保存 /proc/cpuinfo 中单个逻辑 CPU 的信息。
type CPUInfo struct {
	Index      int
	Model      string
	Vendor     string
	MHz        float64
	PhysicalID string
	CoreID     string
	CacheSize  string
	Siblings   string
	Cores      string
}

// ParseCPUInfo 解析 /proc/cpuinfo。
//
// 输入以空行分隔的多个逻辑 CPU 块，每块形如：
//
//	processor	: 0
//	vendor_id	: GenuineIntel
//	model name	: Intel(R) Xeon(R) Gold 6130 CPU @ 2.10GHz
//	cpu MHz		: 2100.000
//	physical id	: 0
//	core id		: 0
//	cpu cores	: 16
//	siblings	: 16
//	cache size	: 16384 KB
func ParseCPUInfo(content string) []CPUInfo {
	var infos []CPUInfo
	cur := CPUInfo{Index: -1}

	flush := func() {
		if cur.Index >= 0 {
			infos = append(infos, cur)
		}
		cur = CPUInfo{Index: -1}
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "processor":
			flush()
			cur.Index = int(util.ParseInt64(value))
		case "model name", "Model", "cpu model":
			if cur.Model == "" {
				cur.Model = value
			}
		case "vendor_id", "vendor":
			if cur.Vendor == "" {
				cur.Vendor = value
			}
		case "cpu MHz", "clock":
			if cur.MHz == 0 {
				cur.MHz = util.ParseFloat64(value)
			}
		case "physical id":
			if cur.PhysicalID == "" {
				cur.PhysicalID = value
			}
		case "core id":
			if cur.CoreID == "" {
				cur.CoreID = value
			}
		case "cache size":
			if cur.CacheSize == "" {
				cur.CacheSize = value
			}
		case "siblings":
			cur.Siblings = value
		case "cpu cores":
			cur.Cores = value
		}
	}
	flush()

	return infos
}

// Uname 保存 uname 风格的主机信息。
type Uname struct {
	SysName    string
	Nodename   string
	Release    string
	Version    string
	Machine    string
	DomainName string
}

// ReadUname 从 /proc/sys/kernel 下拼装 uname 信息。
// machine 用 runtime.GOARCH 代替 uname -m（原生编译时两者一致）。
func ReadUname(procRoot string) Uname {
	u := Uname{
		SysName:    readSysValue(procRoot, "ostype"),
		Nodename:   readSysValue(procRoot, "hostname"),
		Release:    readSysValue(procRoot, "osrelease"),
		Version:    readSysValue(procRoot, "version"),
		DomainName: readSysValue(procRoot, "domainname"),
		Machine:    runtime.GOARCH,
	}
	if u.DomainName == "(none)" {
		u.DomainName = ""
	}
	return u
}

// readSysValue 读取 /proc/sys/kernel/<name> 的内容，失败返回 ""。
func readSysValue(procRoot, name string) string {
	s, err := util.ReadString(filepath.Join(procRoot, "sys", "kernel", name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// Exporter 是 CPU / 负载指标的 Prometheus Collector。
//
// 使用率（usage_percent）与速率类指标依赖两次采样的差值，
// 因此 Exporter 会在内存中保存上一次的数据；
// 首次采集时这些指标不会被输出（没有基线，输出 0 会误导告警）。
type Exporter struct {
	procRoot string

	mu       sync.Mutex
	lastCPU  map[string]CPUStats
	lastCtx  uint64
	lastFork uint64
	lastTime time.Time

	cpuSecondsTotal *prometheus.Desc
	cpuUsagePercent *prometheus.Desc
	cpuInfo         *prometheus.Desc
	cpuFrequencyMHz *prometheus.Desc

	load             *prometheus.Desc
	loadPerCore      *prometheus.Desc
	procsRunning     *prometheus.Desc
	procsBlocked     *prometheus.Desc
	contextSwitches  *prometheus.Desc
	forks            *prometheus.Desc
	bootTime         *prometheus.Desc
	uptime           *prometheus.Desc
	entropyAvailable *prometheus.Desc
	unameInfo        *prometheus.Desc

	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 创建 CPU Collector。procRoot 通常是 "/proc"。
func New(procRoot string) *Exporter {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &Exporter{
		procRoot: procRoot,
		lastCPU:  make(map[string]CPUStats),

		cpuSecondsTotal: prometheus.NewDesc(
			namespace+"_cpu_seconds_total",
			"CPU 时间累计秒数（按模式与逻辑 CPU 区分；cpu=\"total\" 为全部 CPU 之和）",
			[]string{"cpu", "mode"}, nil,
		),
		cpuUsagePercent: prometheus.NewDesc(
			namespace+"_cpu_usage_percent",
			"CPU 使用率百分比（两次采样之间的平均 busy 时间占比，不含 idle 与 iowait）",
			[]string{"cpu"}, nil,
		),
		cpuInfo: prometheus.NewDesc(
			namespace+"_cpu_info",
			"逻辑 CPU 静态信息，值恒为 1",
			[]string{"cpu", "model", "vendor", "physical_id", "core_id", "siblings", "cores", "cache"}, nil,
		),
		cpuFrequencyMHz: prometheus.NewDesc(
			namespace+"_cpu_frequency_mhz",
			"逻辑 CPU 当前主频（MHz），由 /proc/cpuinfo 读取，可能为空",
			[]string{"cpu"}, nil,
		),

		load: prometheus.NewDesc(
			namespace+"_load1", "1 分钟平均负载", nil, nil,
		),
		loadPerCore: prometheus.NewDesc(
			namespace+"_load_per_core_ratio",
			"平均负载除以 CPU 核数（便于跨机器比较）",
			[]string{"interval"}, nil,
		),
		procsRunning: prometheus.NewDesc(
			namespace+"_procs_running", "当前处于可运行状态的进程/线程数", nil, nil,
		),
		procsBlocked: prometheus.NewDesc(
			namespace+"_procs_blocked", "当前阻塞在 IO 上的进程/线程数", nil, nil,
		),
		contextSwitches: prometheus.NewDesc(
			namespace+"_context_switches_total", "上下文切换累计次数", nil, nil,
		),
		forks: prometheus.NewDesc(
			namespace+"_forks_total", "创建的进程累计数量", nil, nil,
		),
		bootTime: prometheus.NewDesc(
			namespace+"_boot_time_seconds", "系统开机时间（Unix 时间戳，秒）", nil, nil,
		),
		uptime: prometheus.NewDesc(
			namespace+"_uptime_seconds", "系统运行时长（秒）", nil, nil,
		),
		entropyAvailable: prometheus.NewDesc(
			namespace+"_entropy_available_bits", "内核可用随机熵位数", nil, nil,
		),
		unameInfo: prometheus.NewDesc(
			namespace+"_uname_info", "内核与主机信息，值恒为 1",
			[]string{"sysname", "nodename", "release", "version", "machine", "domainname"}, nil,
		),

		scrapeDuration: prometheus.NewDesc(
			namespace+"_cpu_scrape_duration_seconds", "CPU 指标采集耗时（秒）", nil, nil,
		),
		scrapeError: prometheus.NewDesc(
			namespace+"_cpu_scrape_error", "CPU 指标采集是否出错（1=出错, 0=正常）", nil, nil,
		),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.cpuSecondsTotal
	ch <- e.cpuUsagePercent
	ch <- e.cpuInfo
	ch <- e.cpuFrequencyMHz
	ch <- e.load
	ch <- e.loadPerCore
	ch <- e.procsRunning
	ch <- e.procsBlocked
	ch <- e.contextSwitches
	ch <- e.forks
	ch <- e.bootTime
	ch <- e.uptime
	ch <- e.entropyAvailable
	ch <- e.unameInfo
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	var errs []string

	// ---- /proc/stat：CPU 时间、上下文切换、进程数 ----
	//
	// 读取与基线更新必须在同一个临界区内完成：使用率是"两次采样之差"，
	// 如果先读文件再抢锁，并发抓取（例如两个 Prometheus 同时抓）时
	// 会出现"读到旧值 -> 拿到已被别人更新过的基线"的错位，
	// 导致 Delta 检测到计数器回退而整组使用率指标消失。
	e.mu.Lock()
	content, err := util.ReadString(filepath.Join(e.procRoot, "stat"))
	if err != nil {
		e.mu.Unlock()
		// 读不到 /proc/stat 时 CPU 相关指标全部无法产出，
		// 直接输出错误标记并结束本次采集。
		e.reportErrors(ch, start, 1)
		return
	}
	ps := ParseProcStat(content)
	if len(ps.CPUs) == 0 {
		errs = append(errs, "no cpu line found in /proc/stat")
	}

	names := make([]string, 0, len(ps.CPUs))
	for name := range ps.CPUs {
		names = append(names, name)
	}
	sort.Strings(names)

	// 累计时间是无条件输出的 counter。
	for _, name := range names {
		cur := ps.CPUs[name]
		for i, mode := range cpuModes {
			ch <- prometheus.MustNewConstMetric(
				e.cpuSecondsTotal, prometheus.CounterValue,
				float64(cur.value(i))/UserHz, cpuLabelOf(name), mode,
			)
		}
	}

	// 使用率与速率需要与上一次采样比较。
	now := time.Now()
	secs := now.Sub(e.lastTime).Seconds()
	hasPrev := !e.lastTime.IsZero() && secs > 0
	if hasPrev {
		for _, name := range names {
			prev, ok := e.lastCPU[name]
			if !ok {
				continue
			}
			cur := ps.CPUs[name]
			totalRate, ok := util.Delta(prev.Total(), cur.Total(), secs)
			if !ok {
				continue
			}
			busyRate, _ := util.Delta(prev.BusyTotal(), cur.BusyTotal(), secs)
			ch <- prometheus.MustNewConstMetric(
				e.cpuUsagePercent, prometheus.GaugeValue,
				100*util.SafeDiv(busyRate, totalRate), cpuLabelOf(name),
			)
		}
		if v, ok := util.Delta(e.lastCtx, ps.ContextSw, secs); ok {
			ch <- prometheus.MustNewConstMetric(e.contextSwitches, prometheus.CounterValue, v)
		}
		if v, ok := util.Delta(e.lastFork, ps.Forks, secs); ok {
			ch <- prometheus.MustNewConstMetric(e.forks, prometheus.CounterValue, v)
		}
	}
	// 保存本次基线，供下次计算速率。
	e.lastCPU = ps.CPUs
	e.lastCtx = ps.ContextSw
	e.lastFork = ps.Forks
	e.lastTime = now
	e.mu.Unlock()

	// procs_running / procs_blocked 是瞬时值，无需基线。
	ch <- prometheus.MustNewConstMetric(e.procsRunning, prometheus.GaugeValue, float64(ps.ProcsRunning))
	ch <- prometheus.MustNewConstMetric(e.procsBlocked, prometheus.GaugeValue, float64(ps.ProcsBlocked))

	if ps.BootTime > 0 {
		ch <- prometheus.MustNewConstMetric(e.bootTime, prometheus.GaugeValue, float64(ps.BootTime))
	}

	// ---- /proc/loadavg ----
	if raw, err := util.ReadString(filepath.Join(e.procRoot, "loadavg")); err != nil {
		errs = append(errs, "read loadavg: "+err.Error())
	} else if la, err := ParseLoadAvg(raw); err != nil {
		errs = append(errs, "parse loadavg: "+err.Error())
	} else {
		cores := float64(runtime.NumCPU())
		ch <- prometheus.MustNewConstMetric(e.load, prometheus.GaugeValue, la.Load1)
		ch <- prometheus.MustNewConstMetric(e.loadPerCore, prometheus.GaugeValue, util.SafeDiv(la.Load1, cores), "1m")
		ch <- prometheus.MustNewConstMetric(e.loadPerCore, prometheus.GaugeValue, util.SafeDiv(la.Load5, cores), "5m")
		ch <- prometheus.MustNewConstMetric(e.loadPerCore, prometheus.GaugeValue, util.SafeDiv(la.Load15, cores), "15m")
	}

	// ---- /proc/uptime ----
	if raw, err := util.ReadString(filepath.Join(e.procRoot, "uptime")); err != nil {
		errs = append(errs, "read uptime: "+err.Error())
	} else {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			errs = append(errs, "unexpected uptime format")
		} else {
			ch <- prometheus.MustNewConstMetric(e.uptime, prometheus.GaugeValue, util.ParseFloat64(fields[0]))
		}
	}

	// ---- 熵 ----
	if raw, err := util.ReadString(filepath.Join(e.procRoot, "sys", "kernel", "random", "entropy_avail")); err == nil {
		ch <- prometheus.MustNewConstMetric(e.entropyAvailable, prometheus.GaugeValue, float64(util.ParseUint64(raw)))
	}

	// ---- /proc/cpuinfo ----
	if raw, err := util.ReadString(filepath.Join(e.procRoot, "cpuinfo")); err != nil {
		errs = append(errs, "read cpuinfo: "+err.Error())
	} else {
		for _, info := range ParseCPUInfo(raw) {
			ch <- prometheus.MustNewConstMetric(
				e.cpuInfo, prometheus.GaugeValue, 1,
				strconv.Itoa(info.Index), info.Model, info.Vendor,
				info.PhysicalID, info.CoreID, info.Siblings, info.Cores, info.CacheSize,
			)
			if info.MHz > 0 {
				ch <- prometheus.MustNewConstMetric(
					e.cpuFrequencyMHz, prometheus.GaugeValue, info.MHz, strconv.Itoa(info.Index),
				)
			}
		}
	}

	// ---- uname ----
	u := ReadUname(e.procRoot)
	ch <- prometheus.MustNewConstMetric(
		e.unameInfo, prometheus.GaugeValue, 1,
		u.SysName, u.Nodename, u.Release, u.Version, u.Machine, u.DomainName,
	)

	errValue := 0.0
	if len(errs) > 0 {
		errValue = 1.0
		log.Printf("CPU 指标采集部分失败: %s", strings.Join(errs, "; "))
	}
	e.reportErrors(ch, start, errValue)
}

// cpuLabelOf 把内部 CPU 名称转换为标签值。
func cpuLabelOf(name string) string {
	if name == "total" {
		return "total"
	}
	return strings.TrimPrefix(name, "cpu")
}

// reportErrors 输出采集耗时与错误标记。
func (e *Exporter) reportErrors(ch chan<- prometheus.Metric, start time.Time, errValue float64) {
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
}

// Exists 报告 /proc/stat 是否可读，便于启动时快速失败。
func Exists(procRoot string) bool {
	if procRoot == "" {
		procRoot = "/proc"
	}
	_, err := os.Stat(filepath.Join(procRoot, "stat"))
	return err == nil
}
