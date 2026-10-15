// Package filesystem 采集本机文件系统的容量指标。
//
// 数据来源：
//
//	挂载表（默认 /proc/mounts）  设备、挂载点、文件系统类型、只读标志
//	unix.Statfs(2)              容量、可用空间、inode 数量
//
// 不依赖 `df` 命令，全部通过系统调用完成。支持按挂载点与文件系统类型筛选。
package filesystem

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"

	"node_exporter/internal/util"
)

const namespace = "node"

// DefaultFstypeExclude 是默认排除的伪文件系统类型。
//
// 这些文件系统（内存文件系统、内核接口、容器 overlay 等）的容量没有监控
// 意义，混在一起会产生大量无用的时间序列。传 -filesystem.fstype-exclude=""
// 可关闭该默认行为。
const DefaultFstypeExclude = `^(tmpfs|devtmpfs|devpts|overlay|squashfs|ramfs|proc|sysfs|cgroup2?|securityfs|debugfs|tracefs|pstore|bpf|configfs|fusectl|mqueue|hugetlbfs|nsfs|efivarfs|autofs|binfmt_misc|nfsd|rpc_pipefs|fuse\.lxcfs|fuse\.gvfsd-fuse)$`

// Mount 是挂载表中的一行。
type Mount struct {
	Device     string
	Mountpoint string
	Fstype     string
	ReadOnly   bool
}

// ParseMounts 解析挂载表内容（/proc/mounts 格式）。
//
// 每行形如：
//
//	/dev/sda1 / ext4 rw,relatime 0 0
//	server:/data /mnt/data nfs4 ro,vers=4.2 0 0
//
// 设备名与挂载点中的空格以八进制转义（\040 表示空格）出现，
// 这里会还原成真实字符。字段不足的行会被跳过。
func ParseMounts(content string) []Mount {
	var mounts []Mount

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		opts := strings.Split(fields[3], ",")
		mounts = append(mounts, Mount{
			Device:     unescapeMountField(fields[0]),
			Mountpoint: unescapeMountField(fields[1]),
			Fstype:     fields[2],
			ReadOnly:   hasOption(opts, "ro"),
		})
	}
	return mounts
}

// hasOption 判断挂载选项里是否包含给定选项。
func hasOption(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// unescapeMountField 还原挂载表字段里的八进制转义。
//
// /proc/mounts 会把空格、制表符、换行与反斜杠转义为 \040 \011 \012 \134。
func unescapeMountField(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		oct := s[i+1 : i+4]
		v, err := parseOctal(oct)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}

// parseOctal 解析三位八进制数字。
func parseOctal(s string) (int, error) {
	v := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("非八进制数字: %q", s)
		}
		v = v*8 + int(c-'0')
	}
	return v, nil
}

// Options 是文件系统 Collector 的配置项。
type Options struct {
	// MountsPath 是挂载表路径（默认 /proc/mounts）。
	MountsPath string
	// Filter 用于筛选挂载点（nil 表示全部）。
	Filter *util.Filter
	// FstypeFilter 用于筛选文件系统类型（nil 表示全部）。
	FstypeFilter *util.Filter
	// Statfs 是 statfs 系统调用，测试时可注入替身；为 nil 时使用 unix.Statfs。
	Statfs func(path string, buf *unix.Statfs_t) error
}

// Exporter 是文件系统指标的 Prometheus Collector。
type Exporter struct {
	opts Options

	size        *prometheus.Desc
	free        *prometheus.Desc
	avail       *prometheus.Desc
	used        *prometheus.Desc
	usedPercent *prometheus.Desc
	files       *prometheus.Desc
	filesFree   *prometheus.Desc
	readonly    *prometheus.Desc

	mountsTotal    *prometheus.Desc
	mountsFiltered *prometheus.Desc
	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 使用给定配置创建文件系统 Collector。
func New(opts Options) *Exporter {
	if opts.MountsPath == "" {
		opts.MountsPath = "/proc/mounts"
	}
	if opts.Statfs == nil {
		opts.Statfs = unix.Statfs
	}

	labels := []string{"device", "mountpoint", "fstype"}

	return &Exporter{
		opts: opts,

		size: prometheus.NewDesc(
			namespace+"_filesystem_size_bytes", "文件系统总容量（字节）", labels, nil),
		free: prometheus.NewDesc(
			namespace+"_filesystem_free_bytes", "文件系统空闲空间（字节，含 root 保留）", labels, nil),
		avail: prometheus.NewDesc(
			namespace+"_filesystem_avail_bytes", "非特权用户可用空间（字节）", labels, nil),
		used: prometheus.NewDesc(
			namespace+"_filesystem_used_bytes", "文件系统已用空间（字节，= size - free）", labels, nil),
		usedPercent: prometheus.NewDesc(
			namespace+"_filesystem_used_percent", "文件系统使用率（%，与 df 口径一致）", labels, nil),
		files: prometheus.NewDesc(
			namespace+"_filesystem_files", "文件系统 inode 总数", labels, nil),
		filesFree: prometheus.NewDesc(
			namespace+"_filesystem_files_free", "文件系统空闲 inode 数", labels, nil),
		readonly: prometheus.NewDesc(
			namespace+"_filesystem_readonly", "文件系统是否只读（1=只读, 0=可读写）", labels, nil),

		mountsTotal: prometheus.NewDesc(
			namespace+"_filesystem_mounts", "本次导出的挂载点数量", nil, nil),
		mountsFiltered: prometheus.NewDesc(
			namespace+"_filesystem_mounts_filtered", "被筛选规则排除的挂载点数量", nil, nil),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_filesystem_scrape_duration_seconds", "文件系统指标采集耗时（秒）", nil, nil),
		scrapeError: prometheus.NewDesc(
			namespace+"_filesystem_scrape_error", "文件系统指标采集是否出错（1=出错, 0=正常）", nil, nil),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.size
	ch <- e.free
	ch <- e.avail
	ch <- e.used
	ch <- e.usedPercent
	ch <- e.files
	ch <- e.filesFree
	ch <- e.readonly
	ch <- e.mountsTotal
	ch <- e.mountsFiltered
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
//
// 读取挂载表失败会让 node_filesystem_scrape_error 置为 1；
// 单个挂载点 statfs 失败（权限不足等）只跳过该挂载点，不影响其余序列。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	content, err := util.ReadString(e.opts.MountsPath)
	if err != nil {
		log.Printf("读取挂载表 %s 失败: %v", e.opts.MountsPath, err)
		e.report(ch, start, 1, 0, 0)
		return
	}

	exported, filtered := 0, 0
	for _, m := range ParseMounts(content) {
		if !e.opts.Filter.Match(m.Mountpoint) || !e.opts.FstypeFilter.Match(m.Fstype) {
			filtered++
			continue
		}

		var st unix.Statfs_t
		if err := e.opts.Statfs(m.Mountpoint, &st); err != nil {
			// 挂载点在采集瞬间消失、或权限不足：跳过即可。
			continue
		}
		exported++

		bsize := float64(st.Bsize)
		size := float64(st.Blocks) * bsize
		free := float64(st.Bfree) * bsize
		avail := float64(st.Bavail) * bsize
		used := size - free

		device, mountpoint, fstype := m.Device, m.Mountpoint, m.Fstype

		ch <- prometheus.MustNewConstMetric(e.size, prometheus.GaugeValue, size, device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.free, prometheus.GaugeValue, free, device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.avail, prometheus.GaugeValue, avail, device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.used, prometheus.GaugeValue, used, device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.usedPercent, prometheus.GaugeValue,
			100*util.SafeDiv(size-avail, size), device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.files, prometheus.GaugeValue, float64(st.Files), device, mountpoint, fstype)
		ch <- prometheus.MustNewConstMetric(e.filesFree, prometheus.GaugeValue, float64(st.Ffree), device, mountpoint, fstype)

		readonly := 0.0
		if m.ReadOnly {
			readonly = 1.0
		}
		ch <- prometheus.MustNewConstMetric(e.readonly, prometheus.GaugeValue, readonly, device, mountpoint, fstype)
	}

	e.report(ch, start, 0, exported, filtered)
}

// report 输出采集耗时、错误标记与挂载点计数。
func (e *Exporter) report(ch chan<- prometheus.Metric, start time.Time, errValue float64, exported, filtered int) {
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
	ch <- prometheus.MustNewConstMetric(e.mountsTotal, prometheus.GaugeValue, float64(exported))
	ch <- prometheus.MustNewConstMetric(e.mountsFiltered, prometheus.GaugeValue, float64(filtered))
}

// String 返回采集配置的可读描述，用于启动日志。
func (e *Exporter) String() string {
	return fmt.Sprintf("mounts=%s filter(%s) fstype-exclude(%s)",
		e.opts.MountsPath, e.opts.Filter, e.opts.FstypeFilter)
}
