package mem

import (
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// memInfoSample 摘自真实 /proc/meminfo。
const memInfoSample = `MemTotal:       15663112 kB
MemFree:          670836 kB
MemAvailable:    2508600 kB
Buffers:           55532 kB
Cached:          1987552 kB
SwapCached:         5992 kB
Active:           5000000 kB
Inactive:         4000000 kB
Active(anon):     3000000 kB
Inactive(anon):    500000 kB
Active(file):     2000000 kB
Inactive(file):   3500000 kB
Unevictable:       100000 kB
Mlocked:            50000 kB
SwapTotal:       28803068 kB
SwapFree:        25404288 kB
Dirty:               1000 kB
Writeback:             0 kB
AnonPages:        3500000 kB
Mapped:           200000 kB
Shmem:            300000 kB
Slab:             500000 kB
SReclaimable:     400000 kB
SUnreclaim:       100000 kB
KReclaimable:      80000 kB
PageTables:        40000 kB
KernelStack:       12000 kB
SecPageTables:      2000 kB
NFS_Unstable:         0 kB
Bounce:                0 kB
WritebackTmp:          0 kB
CommitLimit:      30000000 kB
Committed_AS:     8000000 kB
VmallocTotal:   34359738367 kB
VmallocUsed:        50000 kB
VmallocChunk:          0 kB
HardwareCorrupted:     0 kB
AnonHugePages:         0 kB
ShmemHugePages:        0 kB
ShmemPmdMapped:        0 kB
FileHugePages:         0 kB
FilePmdMapped:         0 kB
HugePages_Total:      10
HugePages_Free:        2
HugePages_Rsvd:        0
HugePages_Surplus:     0
Hugepagesize:       2048 kB
Hugetlb:            20480 kB
DirectMap4k:     12345678 kB
`

func TestParseMemInfo(t *testing.T) {
	mi := ParseMemInfo(memInfoSample)

	// 带 kB 的字段应换算成字节。
	if got, want := mi["MemTotal"], uint64(15663112)*1024; got != want {
		t.Errorf("MemTotal = %d，期望 %d", got, want)
	}
	if got, want := mi["MemAvailable"], uint64(2508600)*1024; got != want {
		t.Errorf("MemAvailable = %d，期望 %d", got, want)
	}

	// 不带单位的字段原样返回。
	if got := mi["HugePages_Total"]; got != 10 {
		t.Errorf("HugePages_Total = %d，期望 10", got)
	}
	// Hugepagesize 带 kB，应换算。
	if got, want := mi["Hugepagesize"], uint64(2048*1024); got != want {
		t.Errorf("Hugepagesize = %d，期望 %d", got, want)
	}
	if _, ok := mi["NotAKey"]; ok {
		t.Error("不存在的键不应出现在结果里")
	}
}

func TestParseVMStat(t *testing.T) {
	vs := ParseVMStat("nr_free_pages 12345\npswpin 100\npswpout 250\npgfault 987654\n")
	if vs["pswpin"] != 100 {
		t.Errorf("pswpin = %d，期望 100", vs["pswpin"])
	}
	if vs["pswpout"] != 250 {
		t.Errorf("pswpout = %d，期望 250", vs["pswpout"])
	}
	if vs["pgfault"] != 987654 {
		t.Errorf("pgfault = %d，期望 987654", vs["pgfault"])
	}
}

// gaugeValue 从指标族中取出指定名字的无标签 gauge 值。
func gaugeValue(families []*dto.MetricFamily, name string) (float64, bool) {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			return 0, false
		}
		return mf.GetMetric()[0].GetGauge().GetValue(), true
	}
	return 0, false
}

// TestCollectDerivedMetrics 验证派生指标（used/百分比/swap）计算正确。
func TestCollectDerivedMetrics(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/meminfo", memInfoSample)
	writeFile(t, root+"/vmstat", "pswpin 7\npswpout 9\n")

	exporter := New(root)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	total := float64(15663112 * 1024)
	avail := float64(2508600 * 1024)

	if v, ok := gaugeValue(families, "node_memory_used_bytes"); !ok || v != total-avail {
		t.Errorf("node_memory_used_bytes = %v (ok=%v)，期望 %v", v, ok, total-avail)
	}
	wantPct := 100 * (total - avail) / total
	if v, ok := gaugeValue(families, "node_memory_used_percent"); !ok || v != wantPct {
		t.Errorf("node_memory_used_percent = %v (ok=%v)，期望 %v", v, ok, wantPct)
	}
	wantAvailPct := 100 * avail / total
	if v, ok := gaugeValue(families, "node_memory_available_percent"); !ok || v != wantAvailPct {
		t.Errorf("node_memory_available_percent = %v，期望 %v", v, wantAvailPct)
	}

	swapTotal := float64(28803068 * 1024)
	swapFree := float64(25404288 * 1024)
	if v, ok := gaugeValue(families, "node_swap_used_bytes"); !ok || v != swapTotal-swapFree {
		t.Errorf("node_swap_used_bytes = %v，期望 %v", v, swapTotal-swapFree)
	}
	wantSwapPct := 100 * (swapTotal - swapFree) / swapTotal
	if v, ok := gaugeValue(families, "node_swap_used_percent"); !ok || v != wantSwapPct {
		t.Errorf("node_swap_used_percent = %v，期望 %v", v, wantSwapPct)
	}

	// 换页计数应为 counter。
	for _, mf := range families {
		if mf.GetName() == "node_swap_pages_total" {
			if mf.GetType() != dto.MetricType_COUNTER {
				t.Errorf("node_swap_pages_total 类型 = %v，期望 COUNTER", mf.GetType())
			}
			if len(mf.GetMetric()) != 2 {
				t.Errorf("node_swap_pages_total 有 %d 个样本，期望 in/out 两个", len(mf.GetMetric()))
			}
		}
	}

	// 大页与错误标记。
	if v := labelledGaugeValue(families, "node_hugepages", "kind", "total"); v != 10 {
		t.Errorf("node_hugepages{kind=\"total\"} = %v，期望 10", v)
	}
	if v := labelledGaugeValue(families, "node_hugepages", "kind", "free"); v != 2 {
		t.Errorf("node_hugepages{kind=\"free\"} = %v，期望 2", v)
	}
	if v, ok := gaugeValue(families, "node_hugepages_size_bytes"); !ok || v != 2048*1024 {
		t.Errorf("node_hugepages_size_bytes = %v (ok=%v)，期望 %d", v, ok, 2048*1024)
	}
	if v, ok := gaugeValue(families, "node_memory_scrape_error"); !ok || v != 0 {
		t.Errorf("node_memory_scrape_error = %v，期望 0", v)
	}
}

// TestCollectMemAvailableFallback 验证没有 MemAvailable 的老内核能用
// free+buffers+cached 兜底。
func TestCollectMemAvailableFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/meminfo", "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 150 kB\n")
	writeFile(t, root+"/vmstat", "pswpin 0\npswpout 0\n")

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(root))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	// used = 1000 - (100+50+150) = 700 kB
	if v, ok := gaugeValue(families, "node_memory_used_bytes"); !ok || v != 700*1024 {
		t.Errorf("node_memory_used_bytes = %v，期望 %v", v, 700*1024)
	}
}

// TestCollectMissingMeminfo 验证 meminfo 缺失时只报错、不 panic。
func TestCollectMissingMeminfo(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(t.TempDir()))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	if v, ok := gaugeValue(families, "node_memory_scrape_error"); !ok || v != 1 {
		t.Errorf("node_memory_scrape_error = %v (ok=%v)，期望 1", v, ok)
	}
}

// labelledGaugeValue 按标签取值，用于 node_hugepages{kind=...} 这类带标签的指标。
func labelledGaugeValue(families []*dto.MetricFamily, name, label, want string) float64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == want {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	return -1
}

// writeFile 写入测试用的伪 proc 文件。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}
