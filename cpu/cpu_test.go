package cpu

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// procStatSample 摘自真实 /proc/stat 的内容。
const procStatSample = `cpu  28678722 722 8578239 1309046041 922803 0 3791418 0 0 0
cpu0 1728043 42 603140 81502815 75818 0 3371085 0 0 0
cpu1 235876 43 109223 84094866 14868 0 185869 0 0 0
intr 123456789 0 0
ctxt 987654321
btime 1700000000
processes 456789
procs_running 3
procs_blocked 1
softirq 12345 0 1 2
`

func TestParseProcStat(t *testing.T) {
	ps := ParseProcStat(procStatSample)

	if len(ps.CPUs) != 3 {
		t.Fatalf("解析到 %d 个 CPU 条目，期望 3 (total, cpu0, cpu1)", len(ps.CPUs))
	}

	total, ok := ps.CPUs["total"]
	if !ok {
		t.Fatal("缺少 total 条目")
	}
	if total.User != 28678722 {
		t.Errorf("total.User = %d，期望 28678722", total.User)
	}
	if total.Iowait != 922803 {
		t.Errorf("total.Iowait = %d，期望 922803", total.Iowait)
	}
	if total.IRQ != 0 {
		t.Errorf("total.IRQ = %d，期望 0", total.IRQ)
	}
	if total.SoftIRQ != 3791418 {
		t.Errorf("total.SoftIRQ = %d，期望 3791418", total.SoftIRQ)
	}

	if ps.CPUs["cpu0"].User != 1728043 {
		t.Errorf("cpu0.User = %d，期望 1728043", ps.CPUs["cpu0"].User)
	}
	if ps.ContextSw != 987654321 {
		t.Errorf("ContextSw = %d，期望 987654321", ps.ContextSw)
	}
	if ps.BootTime != 1700000000 {
		t.Errorf("BootTime = %d，期望 1700000000", ps.BootTime)
	}
	if ps.Forks != 456789 {
		t.Errorf("Forks = %d，期望 456789", ps.Forks)
	}
	if ps.ProcsRunning != 3 || ps.ProcsBlocked != 1 {
		t.Errorf("procs = (%d,%d)，期望 (3,1)", ps.ProcsRunning, ps.ProcsBlocked)
	}
}

// TestCPUTotalExcludesGuest 验证 guest 时间不会重复计入总时间。
func TestCPUTotalExcludesGuest(t *testing.T) {
	cs := CPUStats{User: 100, System: 50, Idle: 200, Iowait: 25, Guest: 30, GuestNice: 10}
	want := uint64(375) // 100+50+200+25，不含 guest
	if got := cs.Total(); got != want {
		t.Errorf("Total() = %d，期望 %d（guest 是 user 的子集，不应计入）", got, want)
	}
	if got, want := cs.IdleTotal(), uint64(225); got != want {
		t.Errorf("IdleTotal() = %d，期望 %d", got, want)
	}
	if got, want := cs.BusyTotal(), uint64(150); got != want {
		t.Errorf("BusyTotal() = %d，期望 %d", got, want)
	}
}

func TestParseLoadAvg(t *testing.T) {
	la, err := ParseLoadAvg("0.15 0.73 0.97 5/2207 825988")
	if err != nil {
		t.Fatalf("ParseLoadAvg 失败: %v", err)
	}
	if la.Load1 != 0.15 || la.Load5 != 0.73 || la.Load15 != 0.97 {
		t.Errorf("负载解析错误: %+v", la)
	}
	if la.RunningTotal != 2207 {
		t.Errorf("RunningTotal = %d，期望 2207", la.RunningTotal)
	}

	if _, err := ParseLoadAvg("bad input"); err == nil {
		t.Error("字段不足时应当返回错误")
	}
}

// cpuInfoSample 摘自真实 /proc/cpuinfo。
const cpuInfoSample = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 117
model name	: AMD Ryzen 7 8745HS w/ Radeon 780M Graphics
stepping	: 2
microcode	: 0xa705206
cpu MHz		: 3851.914
cache size	: 1024 KB
physical id	: 0
siblings	: 16
core id		: 0
cpu cores	: 16
flags		: fpu vme de pse tsc

processor	: 1
vendor_id	: AuthenticAMD
model name	: AMD Ryzen 7 8745HS w/ Radeon 780M Graphics
cpu MHz		: 3851.914
physical id	: 0
core id		: 1

`

func TestParseCPUInfo(t *testing.T) {
	infos := ParseCPUInfo(cpuInfoSample)
	if len(infos) != 2 {
		t.Fatalf("解析到 %d 个 CPU，期望 2", len(infos))
	}

	if infos[0].Index != 0 {
		t.Errorf("Index = %d，期望 0", infos[0].Index)
	}
	if infos[0].Model != "AMD Ryzen 7 8745HS w/ Radeon 780M Graphics" {
		t.Errorf("Model = %q", infos[0].Model)
	}
	if infos[0].Vendor != "AuthenticAMD" {
		t.Errorf("Vendor = %q", infos[0].Vendor)
	}
	if infos[0].MHz != 3851.914 {
		t.Errorf("MHz = %v，期望 3851.914", infos[0].MHz)
	}
	if infos[0].CacheSize != "1024 KB" {
		t.Errorf("CacheSize = %q", infos[0].CacheSize)
	}
	if infos[0].CoreID != "0" || infos[0].PhysicalID != "0" {
		t.Errorf("CoreID/PhysicalID = %q/%q", infos[0].CoreID, infos[0].PhysicalID)
	}
	// 第二个块缺少 cache size，应当留空而不是串到上一个。
	if infos[1].CacheSize != "" {
		t.Errorf("第二个 CPU 的 CacheSize = %q，期望空", infos[1].CacheSize)
	}
	if infos[1].CoreID != "1" {
		t.Errorf("第二个 CPU 的 CoreID = %q，期望 1", infos[1].CoreID)
	}
}

// writeProcRoot 构造一个最小可用的 /proc 目录。
func writeProcRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"stat":                            procStatSample,
		"loadavg":                         "0.15 0.73 0.97 5/2207 825988",
		"uptime":                          "12345.67 98765.43",
		"cpuinfo":                         cpuInfoSample,
		"meminfo":                         "MemTotal: 1024 kB\n",
		"sys/kernel/hostname":             "test-host\n",
		"sys/kernel/ostype":               "Linux\n",
		"sys/kernel/osrelease":            "6.12.0\n",
		"sys/kernel/version":              "#1 SMP\n",
		"sys/kernel/domainname":           "(none)\n",
		"sys/kernel/random/entropy_avail": "1234",
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", path, err)
		}
	}
	return root
}

// TestCollectFakeProcRoot 验证在构造的 /proc 上能正常采集，且指标名与标签正确。
func TestCollectFakeProcRoot(t *testing.T) {
	root := writeProcRoot(t)
	exporter := New(root)

	// 用 pedantic registry 顺带校验 Describe 与 Collect 一致。
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(exporter); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	got := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			switch mf.GetName() {
			case "node_load1", "node_uptime_seconds", "node_entropy_available_bits",
				"node_boot_time_seconds", "node_cpu_scrape_error", "node_procs_running",
				"node_procs_blocked":
				if m.GetGauge() != nil {
					got[mf.GetName()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	if got["node_load1"] != 0.15 {
		t.Errorf("node_load1 = %v，期望 0.15", got["node_load1"])
	}
	if got["node_uptime_seconds"] != 12345.67 {
		t.Errorf("node_uptime_seconds = %v，期望 12345.67", got["node_uptime_seconds"])
	}
	if got["node_entropy_available_bits"] != 1234 {
		t.Errorf("node_entropy_available_bits = %v，期望 1234", got["node_entropy_available_bits"])
	}
	if got["node_boot_time_seconds"] != 1700000000 {
		t.Errorf("node_boot_time_seconds = %v，期望 1700000000", got["node_boot_time_seconds"])
	}
	if got["node_cpu_scrape_error"] != 0 {
		t.Errorf("node_cpu_scrape_error = %v，期望 0", got["node_cpu_scrape_error"])
	}
	if got["node_procs_running"] != 3 {
		t.Errorf("node_procs_running = %v，期望 3", got["node_procs_running"])
	}

	// 首次采集不应输出使用率（没有基线）。
	for _, mf := range families {
		if mf.GetName() == "node_cpu_usage_percent" {
			t.Error("首次采集不应输出 node_cpu_usage_percent")
		}
	}

	// CPU 时间与 uname 必须存在。
	var hasCPUSeconds, hasUname bool
	for _, mf := range families {
		if mf.GetName() == "node_cpu_seconds_total" {
			hasCPUSeconds = true
		}
		if mf.GetName() == "node_uname_info" {
			hasUname = true
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				if lp.GetName() == "nodename" && lp.GetValue() != "test-host" {
					t.Errorf("nodename = %q，期望 test-host", lp.GetValue())
				}
			}
		}
	}
	if !hasCPUSeconds {
		t.Error("缺少 node_cpu_seconds_total")
	}
	if !hasUname {
		t.Error("缺少 node_uname_info")
	}
}

// TestUsagePercentNeedsTwoScrapes 验证使用率在第二次采集后才出现。
func TestUsagePercentNeedsTwoScrapes(t *testing.T) {
	root := t.TempDir()
	statPath := filepath.Join(root, "stat")

	write := func(user, idle uint64) {
		content := "cpu  " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n" +
			"cpu0 " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n"
		if err := os.WriteFile(statPath, []byte(content), 0o644); err != nil {
			t.Fatalf("写入 stat 失败: %v", err)
		}
	}
	for path, content := range map[string]string{
		"loadavg": "0 0 0 1/1 1",
		"uptime":  "1.0 1.0",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", path, err)
		}
	}

	exporter := New(root)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)

	// 第一次：没有使用率。
	write(100, 100)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}
	if hasUsage(families) {
		t.Error("首次采集不应有使用率")
	}

	// 第二次：有使用率，且 busy 占比 100%。
	write(200, 100)
	families, err = reg.Gather()
	if err != nil {
		t.Fatalf("二次 Gather 失败: %v", err)
	}
	if !hasUsage(families) {
		t.Fatal("第二次采集应有使用率")
	}
	for _, mf := range families {
		if mf.GetName() != "node_cpu_usage_percent" {
			continue
		}
		for _, m := range mf.GetMetric() {
			v := m.GetGauge().GetValue()
			if v < 99.9 || v > 100.1 {
				t.Errorf("使用率 = %v，期望接近 100", v)
			}
		}
	}
}

// hasUsage 判断是否存在 node_cpu_usage_percent。
func hasUsage(families []*dto.MetricFamily) bool {
	for _, mf := range families {
		if mf.GetName() == "node_cpu_usage_percent" {
			return true
		}
	}
	return false
}

// itoa 是 strconv.FormatUint 的简写。
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

// TestConcurrentCollectKeepsRates 回归测试：并发抓取时使用率指标不能消失。
//
// 问题背景：曾经把"读 /proc/stat"放在锁外、"更新基线"放在锁内。
// 并发抓取（例如两个 Prometheus 同时抓同一台机器）时，后进入临界区的采集
// 会拿着"自己读到的旧值"去和"别人刚写入的较新基线"作比较，
// Delta 检测到计数器回退，于是整组 node_cpu_usage_percent 消失，
// Prometheus 上表现为曲线断点。
//
// 复现该问题需要两个条件：
//  1. /proc/stat 里的计数在采集期间持续增长（用后台 goroutine 模拟）
//  2. 文件替换必须是原子的（真实 /proc 由内核保证）
func TestConcurrentCollectKeepsRates(t *testing.T) {
	root := t.TempDir()
	statPath := filepath.Join(root, "stat")

	// 计数器基线：与下面写文件的内容使用同一套数值，保证只增不减。
	// （若初值与后续值不连续，会被当成"计数器被重置"，
	//   Delta 按设计会跳过速率指标，测试就会出现假失败。）
	const (
		baseUser = uint64(1_000_000)
		baseIdle = uint64(9_000_000)
	)
	statContent := func(tick uint64) string {
		user := baseUser + tick*10
		idle := baseIdle + tick*10
		return "cpu  " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n" +
			"cpu0 " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\n" +
			"ctxt " + itoa(500_000+tick*7) + "\n" +
			"processes " + itoa(300_000+tick*3) + "\n" +
			"procs_running 1\nprocs_blocked 0\n"
	}

	for path, content := range map[string]string{
		"loadavg": "0.10 0.20 0.30 1/100 2000",
		"uptime":  "1234.5 6789.0",
		"cpuinfo": cpuInfoSample,
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", path, err)
		}
	}
	if err := writeFileAtomic(statPath, statContent(0)); err != nil {
		t.Fatalf("初始化 stat 失败: %v", err)
	}

	// 后台推进计数器。
	stop := make(chan struct{})
	var writerDone sync.WaitGroup
	writerDone.Add(1)
	go func() {
		defer writerDone.Done()
		for tick := uint64(1); ; tick++ {
			select {
			case <-stop:
				return
			default:
			}
			// 尽量快地推进计数器，确保并发采集读到的是不同的取值。
			if err := writeFileAtomic(statPath, statContent(tick)); err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stop)
		writerDone.Wait()
	}()

	// 先采集一次建立基线（此时计数器已经在增长）。
	time.Sleep(2 * time.Millisecond)
	exporter := New(root)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}

	const workers = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		bad   int
		total int
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		// 错开启动时间，确保各采集读到的计数器取值不同，
		// 这样"先读后抢锁"的实现才会真正出现基线错位。
		go func(delay time.Duration) {
			defer wg.Done()
			time.Sleep(delay)
			families, err := reg.Gather()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("并发 Gather 失败: %v", err)
				return
			}
			total++
			if !hasUsage(families) {
				bad++
			}
		}(time.Duration(i) * 300 * time.Microsecond)
	}
	wg.Wait()

	if total != workers {
		t.Fatalf("只完成了 %d 次采集，期望 %d", total, workers)
	}
	if bad != 0 {
		t.Errorf("有 %d/%d 次并发采集缺少 node_cpu_usage_percent（基线更新必须与读取同锁）", bad, total)
	}
}

// writeFileAtomic 通过"先写临时文件再 rename"的方式原子替换文件内容，
// 避免读取方看到截断中的中间状态。
func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
