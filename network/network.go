// Package network 采集网卡流量指标（字节/包/错误/丢包 + 每秒速率 + 链路状态），
// 数据来自 /proc/net/dev 与 /sys/class/net/<iface>/。
//
// 支持按网卡名做包含/排除筛选，便于在同时存在物理网卡、
// 网桥与 veth 的机器上只关注关心的网卡。
package network

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// DefaultExcludeRegex 是默认排除的虚拟网卡。
//
// 这些网卡（回环、容器网桥、veth、VPN 隧道等）的流量通常是本机内部
// 产生的，混在一起会让物理网卡的流量曲线失真，因此默认排除。
// 传 -network.exclude="" 即可关闭该默认行为。
const DefaultExcludeRegex = `^(lo|veth.*|docker[0-9]*|br-.*|virbr.*|tun[0-9]*|tap[0-9]*|zt.*|sit.*|erspan.*|gre.*|ip6tnl.*|wg[0-9]*)$`

// Interface 保存单个网卡的累计计数。
type Interface struct {
	Name  string
	Stats [16]uint64
}

// /proc/net/dev 中收发各 8 个字段：
//
//	0 bytes 1 packets 2 errs 3 drop 4 fifo 5 frame 6 compressed 7 multicast
//	8 bytes 9 packets 10 errs 11 drop 12 fifo 13 colls 14 carrier 15 compressed
const (
	rxBytes = iota
	rxPackets
	rxErrs
	rxDrop
	rxFifo
	rxFrame
	rxCompressed
	rxMulticast
	txBytes
	txPackets
	txErrs
	txDrop
	txFifo
	txColls
	txCarrier
	txCompressed
)

// rxDropReasons 把接收丢包的各种原因映射为标签值。
var rxDropReasons = []struct {
	index int
	kind  string
}{
	{rxDrop, "total"},
	{rxFifo, "fifo"},
	{rxFrame, "frame"},
	{rxCompressed, "compressed"},
	{rxMulticast, "multicast"},
}

// txDropReasons 把发送丢包的各种原因映射为标签值。
var txDropReasons = []struct {
	index int
	kind  string
}{
	{txDrop, "total"},
	{txFifo, "fifo"},
	{txColls, "collisions"},
	{txCarrier, "carrier"},
	{txCompressed, "compressed"},
}

// ParseNetDev 解析 /proc/net/dev 的内容。
//
// 输入形如：
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes  packets errs drop fifo frame compressed multicast|bytes  packets errs drop fifo colls carrier compressed
//	    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
//	  eth0: 900000 900 1 2 0 0 0 0 800000 800 0 1 0 0 0 0
//
// 表头两行会被跳过；只保留字段完整的行。
func ParseNetDev(content string) ([]Interface, error) {
	var ifaces []Interface
	seenData := false

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// 表头行不含 ":"，跳过，直到遇到第一行数据。
		name, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		seenData = true

		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if name == "" || len(fields) < 16 {
			continue
		}

		var iface Interface
		iface.Name = name
		for i := 0; i < 16; i++ {
			iface.Stats[i] = util.ParseUint64(fields[i])
		}
		ifaces = append(ifaces, iface)
	}

	if !seenData {
		return nil, fmt.Errorf("unexpected /proc/net/dev format")
	}
	// 按名称排序，保证输出顺序稳定。
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })
	return ifaces, nil
}

// LinkStatus 保存网卡的链路信息。
type LinkStatus struct {
	SpeedMbps  *float64 // nil 表示无法确定（例如虚拟网卡、carrier 未起）
	Duplex     string   // full / half / unknown
	Carrier    *float64 // 1 = link up, 0 = link down
	OperState  string   // up / down / unknown 等
	MacAddress string
}

// ReadLinkStatus 从 /sys/class/net/<name>/ 读取链路信息。
// 虚拟网卡通常没有 speed/duplex 文件，此时相应字段留空。
func ReadLinkStatus(sysRoot, name string) LinkStatus {
	base := filepath.Join(sysRoot, "class", "net", name)
	st := LinkStatus{Duplex: "unknown", OperState: "unknown"}

	if s, err := util.ReadString(filepath.Join(base, "operstate")); err == nil {
		if v := strings.TrimSpace(s); v != "" {
			st.OperState = v
		}
	}
	if s, err := util.ReadString(filepath.Join(base, "address")); err == nil {
		st.MacAddress = strings.TrimSpace(s)
	}
	// speed 单位是 Mbps，虚拟网卡读出来是 -1 或直接报错。
	if s, err := util.ReadString(filepath.Join(base, "speed")); err == nil {
		if v := util.ParseFloat64(s); v > 0 {
			st.SpeedMbps = &v
		}
	}
	if s, err := util.ReadString(filepath.Join(base, "duplex")); err == nil {
		if v := strings.TrimSpace(s); v == "full" || v == "half" {
			st.Duplex = v
		}
	}
	if s, err := util.ReadString(filepath.Join(base, "carrier")); err == nil {
		if v := util.ParseFloat64(s); v == 0 || v == 1 {
			st.Carrier = &v
		}
	}
	return st
}

// Exporter 是网卡流量指标的 Prometheus Collector。
type Exporter struct {
	procRoot string
	sysRoot  string
	filter   *util.Filter

	mu       sync.Mutex
	lastTime time.Time
	last     map[string][16]uint64

	recvBytes   *prometheus.Desc
	recvPackets *prometheus.Desc
	recvErrors  *prometheus.Desc
	recvDropped *prometheus.Desc
	sendBytes   *prometheus.Desc
	sendPackets *prometheus.Desc
	sendErrors  *prometheus.Desc
	sendDropped *prometheus.Desc

	recvBytesRate   *prometheus.Desc
	sendBytesRate   *prometheus.Desc
	recvPacketsRate *prometheus.Desc
	sendPacketsRate *prometheus.Desc

	linkSpeed   *prometheus.Desc
	linkDuplex  *prometheus.Desc
	linkCarrier *prometheus.Desc
	linkInfo    *prometheus.Desc

	interfaceCount *prometheus.Desc
	filteredCount  *prometheus.Desc
	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 创建网卡流量 Collector。
// filter 为 nil 时不过滤（导出全部网卡）。
func New(procRoot, sysRoot string, filter *util.Filter) *Exporter {
	if procRoot == "" {
		procRoot = "/proc"
	}
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	return &Exporter{
		procRoot: procRoot,
		sysRoot:  sysRoot,
		filter:   filter,
		last:     make(map[string][16]uint64),

		recvBytes: prometheus.NewDesc(
			namespace+"_network_receive_bytes_total",
			"网卡累计接收字节数", []string{"device"}, nil,
		),
		recvPackets: prometheus.NewDesc(
			namespace+"_network_receive_packets_total",
			"网卡累计接收包数", []string{"device"}, nil,
		),
		recvErrors: prometheus.NewDesc(
			namespace+"_network_receive_errors_total",
			"网卡累计接收错误数", []string{"device"}, nil,
		),
		recvDropped: prometheus.NewDesc(
			namespace+"_network_receive_dropped_total",
			"网卡累计接收丢包数，按原因区分",
			[]string{"device", "reason"}, nil,
		),
		sendBytes: prometheus.NewDesc(
			namespace+"_network_transmit_bytes_total",
			"网卡累计发送字节数", []string{"device"}, nil,
		),
		sendPackets: prometheus.NewDesc(
			namespace+"_network_transmit_packets_total",
			"网卡累计发送包数", []string{"device"}, nil,
		),
		sendErrors: prometheus.NewDesc(
			namespace+"_network_transmit_errors_total",
			"网卡累计发送错误数", []string{"device"}, nil,
		),
		sendDropped: prometheus.NewDesc(
			namespace+"_network_transmit_dropped_total",
			"网卡累计发送丢包数，按原因区分",
			[]string{"device", "reason"}, nil,
		),

		recvBytesRate: prometheus.NewDesc(
			namespace+"_network_receive_bytes_per_second",
			"网卡接收速率（字节/秒，两次采样之间的平均值）", []string{"device"}, nil,
		),
		sendBytesRate: prometheus.NewDesc(
			namespace+"_network_transmit_bytes_per_second",
			"网卡发送速率（字节/秒，两次采样之间的平均值）", []string{"device"}, nil,
		),
		recvPacketsRate: prometheus.NewDesc(
			namespace+"_network_receive_packets_per_second",
			"网卡接收速率（包/秒，两次采样之间的平均值）", []string{"device"}, nil,
		),
		sendPacketsRate: prometheus.NewDesc(
			namespace+"_network_transmit_packets_per_second",
			"网卡发送速率（包/秒，两次采样之间的平均值）", []string{"device"}, nil,
		),

		linkSpeed: prometheus.NewDesc(
			namespace+"_network_link_speed_bps",
			"网卡协商速率（比特/秒），虚拟网卡通常无此值", []string{"device"}, nil,
		),
		linkDuplex: prometheus.NewDesc(
			namespace+"_network_link_duplex",
			"网卡双工模式：1=full, 0=half, 0.5=未知", []string{"device"}, nil,
		),
		linkCarrier: prometheus.NewDesc(
			namespace+"_network_link_carrier",
			"网卡物理链路状态：1=up, 0=down（虚拟网卡无此值时不输出）", []string{"device"}, nil,
		),
		linkInfo: prometheus.NewDesc(
			namespace+"_network_info",
			"网卡信息，值恒为 1",
			[]string{"device", "operstate", "duplex", "mac_address"}, nil,
		),

		interfaceCount: prometheus.NewDesc(
			namespace+"_network_interfaces",
			"本次导出的网卡数量", nil, nil,
		),
		filteredCount: prometheus.NewDesc(
			namespace+"_network_interfaces_filtered",
			"被筛选规则排除的网卡数量（可用于确认规则是否生效）", nil, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_network_scrape_duration_seconds", "网卡指标采集耗时（秒）", nil, nil,
		),
		scrapeError: prometheus.NewDesc(
			namespace+"_network_scrape_error", "网卡指标采集是否出错（1=出错, 0=正常）", nil, nil,
		),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.recvBytes
	ch <- e.recvPackets
	ch <- e.recvErrors
	ch <- e.recvDropped
	ch <- e.sendBytes
	ch <- e.sendPackets
	ch <- e.sendErrors
	ch <- e.sendDropped
	ch <- e.recvBytesRate
	ch <- e.sendBytesRate
	ch <- e.recvPacketsRate
	ch <- e.sendPacketsRate
	ch <- e.linkSpeed
	ch <- e.linkDuplex
	ch <- e.linkCarrier
	ch <- e.linkInfo
	ch <- e.interfaceCount
	ch <- e.filteredCount
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	// 读取、速率计算与基线更新必须在同一个临界区内完成。
	// 否则并发抓取时会出现"读到旧计数 -> 基线已被其他抓取更新"的错位，
	// 使速率算成 0 甚至整组缺失。
	e.mu.Lock()
	content, err := util.ReadString(filepath.Join(e.procRoot, "net", "dev"))
	if err != nil {
		e.mu.Unlock()
		log.Printf("网卡指标采集失败: %v", err)
		e.reportErrors(ch, start, 1, 0, 0)
		return
	}
	ifaces, err := ParseNetDev(content)
	if err != nil {
		e.mu.Unlock()
		log.Printf("解析 %s/net/dev 失败: %v", e.procRoot, err)
		e.reportErrors(ch, start, 1, 0, 0)
		return
	}

	now := time.Now()
	secs := now.Sub(e.lastTime).Seconds()
	hasPrev := !e.lastTime.IsZero() && secs > 0
	prev := e.last
	current := make(map[string][16]uint64, len(ifaces))

	exported, filtered := 0, 0
	for _, iface := range ifaces {
		if !e.filter.Match(iface.Name) {
			filtered++
			continue
		}
		exported++
		current[iface.Name] = iface.Stats
		name := iface.Name
		s := iface.Stats

		// ---- 累计计数 ----
		ch <- prometheus.MustNewConstMetric(e.recvBytes, prometheus.CounterValue, float64(s[rxBytes]), name)
		ch <- prometheus.MustNewConstMetric(e.recvPackets, prometheus.CounterValue, float64(s[rxPackets]), name)
		ch <- prometheus.MustNewConstMetric(e.recvErrors, prometheus.CounterValue, float64(s[rxErrs]), name)
		ch <- prometheus.MustNewConstMetric(e.sendBytes, prometheus.CounterValue, float64(s[txBytes]), name)
		ch <- prometheus.MustNewConstMetric(e.sendPackets, prometheus.CounterValue, float64(s[txPackets]), name)
		ch <- prometheus.MustNewConstMetric(e.sendErrors, prometheus.CounterValue, float64(s[txErrs]), name)

		for _, r := range rxDropReasons {
			ch <- prometheus.MustNewConstMetric(e.recvDropped, prometheus.CounterValue, float64(s[r.index]), name, r.kind)
		}
		for _, r := range txDropReasons {
			ch <- prometheus.MustNewConstMetric(e.sendDropped, prometheus.CounterValue, float64(s[r.index]), name, r.kind)
		}

		// ---- 每秒速率 ----
		if old, ok := prev[name]; ok && hasPrev {
			if v, ok := util.Delta(old[rxBytes], s[rxBytes], secs); ok {
				ch <- prometheus.MustNewConstMetric(e.recvBytesRate, prometheus.GaugeValue, v, name)
			}
			if v, ok := util.Delta(old[txBytes], s[txBytes], secs); ok {
				ch <- prometheus.MustNewConstMetric(e.sendBytesRate, prometheus.GaugeValue, v, name)
			}
			if v, ok := util.Delta(old[rxPackets], s[rxPackets], secs); ok {
				ch <- prometheus.MustNewConstMetric(e.recvPacketsRate, prometheus.GaugeValue, v, name)
			}
			if v, ok := util.Delta(old[txPackets], s[txPackets], secs); ok {
				ch <- prometheus.MustNewConstMetric(e.sendPacketsRate, prometheus.GaugeValue, v, name)
			}
		}

		// ---- 链路状态 ----
		st := ReadLinkStatus(e.sysRoot, name)
		ch <- prometheus.MustNewConstMetric(e.linkInfo, prometheus.GaugeValue, 1,
			name, st.OperState, st.Duplex, st.MacAddress)
		if st.SpeedMbps != nil {
			ch <- prometheus.MustNewConstMetric(e.linkSpeed, prometheus.GaugeValue, *st.SpeedMbps*1e6, name)
		}
		switch st.Duplex {
		case "full":
			ch <- prometheus.MustNewConstMetric(e.linkDuplex, prometheus.GaugeValue, 1, name)
		case "half":
			ch <- prometheus.MustNewConstMetric(e.linkDuplex, prometheus.GaugeValue, 0, name)
		default:
			// 用 0.5 表达"未知"，避免与 half（0）混淆。
			ch <- prometheus.MustNewConstMetric(e.linkDuplex, prometheus.GaugeValue, 0.5, name)
		}
		if st.Carrier != nil {
			ch <- prometheus.MustNewConstMetric(e.linkCarrier, prometheus.GaugeValue, *st.Carrier, name)
		}
	}

	// 保存基线与时间戳。注意只保存通过筛选的网卡，
	// 这样调整筛选规则后第一次采集不会有速率突变。
	e.last = current
	e.lastTime = now
	e.mu.Unlock()

	e.reportErrors(ch, start, 0, exported, filtered)
}

// reportErrors 输出采集耗时、错误标记与网卡计数。
func (e *Exporter) reportErrors(ch chan<- prometheus.Metric, start time.Time, errValue float64, exported, filtered int) {
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
	ch <- prometheus.MustNewConstMetric(e.interfaceCount, prometheus.GaugeValue, float64(exported))
	ch <- prometheus.MustNewConstMetric(e.filteredCount, prometheus.GaugeValue, float64(filtered))
}

// SysRootExists 检查 sysfs 中是否能找到网卡目录。
func SysRootExists(sysRoot string) bool {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	_, err := os.Stat(filepath.Join(sysRoot, "class", "net"))
	return err == nil
}
