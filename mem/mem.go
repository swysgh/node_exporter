// Package mem 采集内存与交换分区（swap）指标，数据来自
// /proc/meminfo 与 /proc/vmstat。
package mem

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// memFields 是要导出的内存字段。
//
// 表结构为：指标后缀、/proc/meminfo 中的键名、单位。
// meminfo 中带 "kB" 单位的字段一律换算成字节，因此后缀统一为 _bytes；
// HugePages_* 是页数而非字节，单独在下面用计数器导出。
var memFields = []struct {
	suffix string
	key    string
}{
	{"total_bytes", "MemTotal"},
	{"free_bytes", "MemFree"},
	{"available_bytes", "MemAvailable"},
	{"buffers_bytes", "Buffers"},
	{"cached_bytes", "Cached"},
	{"swap_cached_bytes", "SwapCached"},
	{"shmem_bytes", "Shmem"},
	{"slab_bytes", "Slab"},
	{"sreclaimable_bytes", "SReclaimable"},
	{"sunreclaim_bytes", "SUnreclaim"},
	{"page_tables_bytes", "PageTables"},
	{"kernel_stack_bytes", "KernelStack"},
	{"anon_pages_bytes", "AnonPages"},
	{"mapped_bytes", "Mapped"},
	{"shmem_hugepages_bytes", "ShmemHugePages"},
	{"dirty_bytes", "Dirty"},
	{"writeback_bytes", "Writeback"},
	{"writeback_tmp_bytes", "WritebackTmp"},
	{"active_bytes", "Active"},
	{"inactive_bytes", "Inactive"},
	{"active_anon_bytes", "Active_anon"},
	{"inactive_anon_bytes", "Inactive_anon"},
	{"active_file_bytes", "Active_file"},
	{"inactive_file_bytes", "Inactive_file"},
	{"unevictable_bytes", "Unevictable"},
	{"locked_bytes", "Locked"},
	{"commit_limit_bytes", "CommitLimit"},
	{"committed_as_bytes", "Committed_AS"},
	{"vmalloc_used_bytes", "VmallocUsed"},
	{"vmalloc_chunk_bytes", "VmallocChunk"},
	{"hardware_corrupted_bytes", "HardwareCorrupted"},
}

// swapFields 是要导出的 swap 字段（同样来自 /proc/meminfo）。
var swapFields = []struct {
	suffix string
	key    string
}{
	{"total_bytes", "SwapTotal"},
	{"free_bytes", "SwapFree"},
	{"cached_bytes", "SwapCached"},
}

// hugePageFields 是大页统计，单位为"页"（不是字节）。
var hugePageFields = []struct {
	suffix string
	key    string
}{
	{"total", "HugePages_Total"},
	{"free", "HugePages_Free"},
	{"reserved", "HugePages_Rsvd"},
	{"surplus", "HugePages_Surplus"},
}

// ParseMemInfo 解析 /proc/meminfo，返回键到字节数的映射。
//
// 输入形如：
//
//	MemTotal:       16316560 kB
//	MemFree:         1234567 kB
//	MemAvailable:   12345678 kB
//
// 带 "kB" 单位的值乘以 1024 换算成字节；不带单位的字段（如
// HugePages_Total、Numa_ 相关的计数）原样返回。
// 缺省为 0 的键（"Hugepagesize" 之外的 Numa 统计）不会自动补齐，
// 缺失的键在导出时会被跳过。
func ParseMemInfo(content string) map[string]uint64 {
	out := make(map[string]uint64, 128)

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value := util.ParseUint64(fields[0])
		if len(fields) > 1 && fields[1] == "kB" {
			value *= 1024
		}
		out[strings.TrimSpace(key)] = value
	}
	return out
}

// ParseVMStat 解析 /proc/vmstat，返回键到数值的映射。
// 只关心 swap 换页相关的 pswpin / pswpout。
func ParseVMStat(content string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, line := range util.SplitLines(content) {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = util.ParseUint64(strings.TrimSpace(value))
	}
	return out
}

// Exporter 是内存 / swap 指标的 Prometheus Collector。
type Exporter struct {
	procRoot string

	memoryDesc    map[string]*prometheus.Desc // 后缀 -> Desc
	memoryUsed    *prometheus.Desc
	memoryUsedPct *prometheus.Desc
	availPct      *prometheus.Desc
	sharedDesc    *prometheus.Desc

	swapDesc    map[string]*prometheus.Desc
	swapUsed    *prometheus.Desc
	swapUsedPct *prometheus.Desc
	swapInOut   *prometheus.Desc

	hugePages *prometheus.Desc
	pageSize  *prometheus.Desc

	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 创建内存 Collector。procRoot 通常是 "/proc"。
func New(procRoot string) *Exporter {
	if procRoot == "" {
		procRoot = "/proc"
	}

	e := &Exporter{
		procRoot:   procRoot,
		memoryDesc: make(map[string]*prometheus.Desc, len(memFields)),
		swapDesc:   make(map[string]*prometheus.Desc, len(swapFields)),
	}

	for _, f := range memFields {
		e.memoryDesc[f.suffix] = prometheus.NewDesc(
			namespace+"_memory_"+f.suffix,
			fmt.Sprintf("内存 %s（来自 /proc/meminfo 的 %s）", f.suffix, f.key),
			nil, nil,
		)
	}
	for _, f := range swapFields {
		e.swapDesc[f.suffix] = prometheus.NewDesc(
			namespace+"_swap_"+f.suffix,
			fmt.Sprintf("交换分区 %s（来自 /proc/meminfo 的 %s）", f.suffix, f.key),
			nil, nil,
		)
	}

	e.memoryUsed = prometheus.NewDesc(
		namespace+"_memory_used_bytes",
		"已使用内存 = MemTotal - MemAvailable", nil, nil,
	)
	e.memoryUsedPct = prometheus.NewDesc(
		namespace+"_memory_used_percent",
		"内存使用率（%），基于 MemAvailable 计算（与 free 命令的 used 口径一致）", nil, nil,
	)
	e.availPct = prometheus.NewDesc(
		namespace+"_memory_available_percent",
		"可用内存占比（%）", nil, nil,
	)
	e.sharedDesc = prometheus.NewDesc(
		namespace+"_memory_shared_bytes",
		"共享内存（Shmem）字节数", nil, nil,
	)
	e.swapUsed = prometheus.NewDesc(
		namespace+"_swap_used_bytes",
		"已使用交换分区 = SwapTotal - SwapFree", nil, nil,
	)
	e.swapUsedPct = prometheus.NewDesc(
		namespace+"_swap_used_percent",
		"交换分区使用率（%）", nil, nil,
	)
	e.swapInOut = prometheus.NewDesc(
		namespace+"_swap_pages_total",
		"换页统计（页数）：out=换出到 swap，in=从 swap 换入",
		[]string{"direction"}, nil,
	)
	e.hugePages = prometheus.NewDesc(
		namespace+"_hugepages",
		"大页统计（单位：页）",
		[]string{"kind"}, nil,
	)
	e.pageSize = prometheus.NewDesc(
		namespace+"_hugepages_size_bytes",
		"单个大页的字节数", nil, nil,
	)

	e.scrapeDuration = prometheus.NewDesc(
		namespace+"_memory_scrape_duration_seconds", "内存指标采集耗时（秒）", nil, nil,
	)
	e.scrapeError = prometheus.NewDesc(
		namespace+"_memory_scrape_error", "内存指标采集是否出错（1=出错, 0=正常）", nil, nil,
	)
	return e
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, f := range memFields {
		ch <- e.memoryDesc[f.suffix]
	}
	for _, f := range swapFields {
		ch <- e.swapDesc[f.suffix]
	}
	ch <- e.memoryUsed
	ch <- e.memoryUsedPct
	ch <- e.availPct
	ch <- e.sharedDesc
	ch <- e.swapUsed
	ch <- e.swapUsedPct
	ch <- e.swapInOut
	ch <- e.hugePages
	ch <- e.pageSize
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	var errs []string

	content, err := util.ReadString(filepath.Join(e.procRoot, "meminfo"))
	if err != nil {
		e.reportErrors(ch, start, 1)
		return
	}
	mi := ParseMemInfo(content)

	// ---- meminfo 中的固定字段 ----
	for _, f := range memFields {
		v, ok := mi[f.key]
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(e.memoryDesc[f.suffix], prometheus.GaugeValue, float64(v))
	}

	total := mi["MemTotal"]
	available := mi["MemAvailable"]
	// 老内核没有 MemAvailable，用 free+buffers+cached 近似，保证指标不缺失。
	if _, ok := mi["MemAvailable"]; !ok {
		available = mi["MemFree"] + mi["Buffers"] + mi["Cached"]
	}

	if total > 0 {
		used := total
		if available <= total {
			used = total - available
		}
		ch <- prometheus.MustNewConstMetric(e.memoryUsed, prometheus.GaugeValue, float64(used))
		ch <- prometheus.MustNewConstMetric(e.memoryUsedPct, prometheus.GaugeValue,
			100*util.SafeDiv(float64(used), float64(total)))
		ch <- prometheus.MustNewConstMetric(e.availPct, prometheus.GaugeValue,
			100*util.SafeDiv(float64(available), float64(total)))
	}
	if v, ok := mi["Shmem"]; ok {
		ch <- prometheus.MustNewConstMetric(e.sharedDesc, prometheus.GaugeValue, float64(v))
	}

	// ---- swap ----
	for _, f := range swapFields {
		v, ok := mi[f.key]
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(e.swapDesc[f.suffix], prometheus.GaugeValue, float64(v))
	}
	swapTotal := mi["SwapTotal"]
	swapFree := mi["SwapFree"]
	if swapTotal > 0 {
		used := swapTotal - swapFree
		if swapFree > swapTotal {
			used = 0
		}
		ch <- prometheus.MustNewConstMetric(e.swapUsed, prometheus.GaugeValue, float64(used))
		ch <- prometheus.MustNewConstMetric(e.swapUsedPct, prometheus.GaugeValue,
			100*util.SafeDiv(float64(used), float64(swapTotal)))
	}

	// ---- 换页次数（累计页数，counter）----
	if raw, err := util.ReadString(filepath.Join(e.procRoot, "vmstat")); err != nil {
		errs = append(errs, "read vmstat: "+err.Error())
	} else {
		vs := ParseVMStat(raw)
		for _, dir := range []struct {
			label string
			key   string
		}{
			{"in", "pswpin"},
			{"out", "pswpout"},
		} {
			if v, ok := vs[dir.key]; ok {
				ch <- prometheus.MustNewConstMetric(e.swapInOut, prometheus.CounterValue, float64(v), dir.label)
			}
		}
	}

	// ---- 大页 ----
	for _, f := range hugePageFields {
		if v, ok := mi[f.key]; ok {
			ch <- prometheus.MustNewConstMetric(e.hugePages, prometheus.GaugeValue, float64(v), f.suffix)
		}
	}
	if v, ok := mi["Hugepagesize"]; ok {
		ch <- prometheus.MustNewConstMetric(e.pageSize, prometheus.GaugeValue, float64(v))
	}

	errValue := 0.0
	if len(errs) > 0 {
		errValue = 1.0
	}
	e.reportErrors(ch, start, errValue)
}

// reportErrors 输出采集耗时与错误标记。
func (e *Exporter) reportErrors(ch chan<- prometheus.Metric, start time.Time, errValue float64) {
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
}
