package network

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"node_exporter/internal/util"
)

// netDevSample 摘自真实 /proc/net/dev。
const netDevSample = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 70455406765 132366608    0    0    0     0          0         0 70455406765 132366608    0    0    0     0       0          0
wlp1s0: 10477457131 19570785    0 112424    0     0          0         0 4235470037 11420840    0  934    0     0       0          0
  eth0:       1000      10    1     2    3     4          5         6       2000      20    7     8    9    10      11         12
 veth1234:     100      1    0    0    0     0          0         0        200       2    0    0    0     0       0          0
`

func TestParseNetDev(t *testing.T) {
	ifaces, err := ParseNetDev(netDevSample)
	if err != nil {
		t.Fatalf("ParseNetDev 失败: %v", err)
	}

	// 应解析出 4 个接口，且按名称排序。
	if len(ifaces) != 4 {
		t.Fatalf("解析到 %d 个接口，期望 4", len(ifaces))
	}
	wantOrder := []string{"eth0", "lo", "veth1234", "wlp1s0"}
	for i, name := range wantOrder {
		if ifaces[i].Name != name {
			t.Errorf("接口[%d] = %q，期望 %q（应按名称排序）", i, ifaces[i].Name, name)
		}
	}

	// 校验字段顺序：接收 8 个 + 发送 8 个。
	eth := ifaces[0]
	if eth.Stats[rxBytes] != 1000 {
		t.Errorf("eth0 rxBytes = %d，期望 1000", eth.Stats[rxBytes])
	}
	if eth.Stats[rxPackets] != 10 {
		t.Errorf("eth0 rxPackets = %d，期望 10", eth.Stats[rxPackets])
	}
	if eth.Stats[rxErrs] != 1 || eth.Stats[rxDrop] != 2 || eth.Stats[rxFifo] != 3 {
		t.Errorf("eth0 接收错误/丢包字段错位: %v", eth.Stats[:8])
	}
	if eth.Stats[rxMulticast] != 6 {
		t.Errorf("eth0 rxMulticast = %d，期望 6", eth.Stats[rxMulticast])
	}
	if eth.Stats[txBytes] != 2000 {
		t.Errorf("eth0 txBytes = %d，期望 2000", eth.Stats[txBytes])
	}
	if eth.Stats[txErrs] != 7 || eth.Stats[txDrop] != 8 {
		t.Errorf("eth0 发送错误/丢包字段错位: %v", eth.Stats[8:])
	}
	if eth.Stats[txColls] != 10 || eth.Stats[txCarrier] != 11 || eth.Stats[txCompressed] != 12 {
		t.Errorf("eth0 发送尾段字段错位: %v", eth.Stats[8:])
	}

	// lo 的收发字节数应相等（量很大，验证没有 int32 溢出）。
	lo := ifaces[1]
	if lo.Stats[rxBytes] != 70455406765 {
		t.Errorf("lo rxBytes = %d，期望 70455406765（超出 int32 范围）", lo.Stats[rxBytes])
	}
	if lo.Stats[txBytes] != 70455406765 {
		t.Errorf("lo txBytes = %d", lo.Stats[txBytes])
	}
}

func TestParseNetDevErrors(t *testing.T) {
	if _, err := ParseNetDev(""); err == nil {
		t.Error("空输入应当返回错误")
	}
	if _, err := ParseNetDev("只有表头没有数据\n"); err == nil {
		t.Error("没有数据行时应当返回错误")
	}

	// 字段不足的行应被跳过而不是 panic。
	ifaces, err := ParseNetDev("  eth0: 100 10\n")
	if err != nil {
		t.Fatalf("字段不足不应返回错误: %v", err)
	}
	if len(ifaces) != 0 {
		t.Errorf("字段不足的行应被跳过，实际解析出 %d 个", len(ifaces))
	}
}

// TestFilterIntegration 验证默认排除规则与自定义规则的效果。
func TestFilterIntegration(t *testing.T) {
	names := []string{"lo", "wlp1s0", "eth0", "veth1234", "docker0", "br-abc"}

	// 默认规则应排除 lo / veth / docker / br-。
	defaultFilter, err := util.NewFilter("", DefaultExcludeRegex)
	if err != nil {
		t.Fatalf("默认排除正则无效: %v", err)
	}
	exported := []string{}
	for _, n := range names {
		if defaultFilter.Match(n) {
			exported = append(exported, n)
		}
	}
	if len(exported) != 2 || exported[0] != "wlp1s0" || exported[1] != "eth0" {
		t.Errorf("默认筛选后 = %v，期望 [wlp1s0 eth0]", exported)
	}

	// include 只放行指定网卡。
	includeFilter, _ := util.NewFilter("^wl", "")
	exported = exported[:0]
	for _, n := range names {
		if includeFilter.Match(n) {
			exported = append(exported, n)
		}
	}
	if len(exported) != 1 || exported[0] != "wlp1s0" {
		t.Errorf("include 筛选后 = %v，期望 [wlp1s0]", exported)
	}

	// 空 exclude 表示不过滤，lo 应当被导出。
	noExclude, _ := util.NewFilter("", "")
	if !noExclude.Match("lo") {
		t.Error("exclude 为空时 lo 应当被导出")
	}
}

// writeFile 写入测试用文件。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

// makeRoots 构造包含 net/dev 与网卡 sysfs 的伪根目录。
func makeRoots(t *testing.T, netDev string) (procRoot, sysRoot string) {
	t.Helper()
	procRoot = t.TempDir()
	sysRoot = t.TempDir()

	writeFile(t, filepath.Join(procRoot, "net", "dev"), netDev)

	// eth0：千兆全双工，链路正常
	writeFile(t, filepath.Join(sysRoot, "class", "net", "eth0", "operstate"), "up\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "eth0", "address"), "aa:bb:cc:dd:ee:ff\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "eth0", "speed"), "1000\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "eth0", "duplex"), "full\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "eth0", "carrier"), "1\n")

	// wlp1s0：无线网卡，没有 speed/duplex 文件
	writeFile(t, filepath.Join(sysRoot, "class", "net", "wlp1s0", "operstate"), "down\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "wlp1s0", "address"), "11:22:33:44:55:66\n")

	// veth1234：虚拟网卡，speed 为 -1
	writeFile(t, filepath.Join(sysRoot, "class", "net", "veth1234", "operstate"), "unknown\n")
	writeFile(t, filepath.Join(sysRoot, "class", "net", "veth1234", "speed"), "-1\n")

	return procRoot, sysRoot
}

func TestReadLinkStatus(t *testing.T) {
	procRoot, sysRoot := makeRoots(t, netDevSample)
	_ = procRoot

	st := ReadLinkStatus(sysRoot, "eth0")
	if st.SpeedMbps == nil || *st.SpeedMbps != 1000 {
		t.Errorf("eth0 speed = %v，期望 1000", st.SpeedMbps)
	}
	if st.Duplex != "full" {
		t.Errorf("eth0 duplex = %q，期望 full", st.Duplex)
	}
	if st.Carrier == nil || *st.Carrier != 1 {
		t.Errorf("eth0 carrier = %v，期望 1", st.Carrier)
	}
	if st.MacAddress != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("eth0 mac = %q", st.MacAddress)
	}

	// 无 speed 文件时应为 nil（指标不输出）。
	st = ReadLinkStatus(sysRoot, "wlp1s0")
	if st.SpeedMbps != nil {
		t.Errorf("wlp1s0 speed 应为 nil，实际 %v", *st.SpeedMbps)
	}
	if st.Duplex != "unknown" {
		t.Errorf("wlp1s0 duplex = %q，期望 unknown", st.Duplex)
	}
	if st.Carrier != nil {
		t.Errorf("wlp1s0 无 carrier 文件时应为 nil")
	}

	// speed 为 -1（虚拟网卡）时也应视为未知。
	st = ReadLinkStatus(sysRoot, "veth1234")
	if st.SpeedMbps != nil {
		t.Errorf("veth0 speed(-1) 应为 nil")
	}
}

// metricWithLabels 判断某个指标族里是否存在满足全部标签条件的样本。
func metricWithLabels(families []*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for k, want := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == want {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return m
			}
		}
	}
	return nil
}

func TestCollect(t *testing.T) {
	procRoot, sysRoot := makeRoots(t, netDevSample)
	filter, _ := util.NewFilter("", DefaultExcludeRegex)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(procRoot, sysRoot, filter))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	// eth0 的计数器值应与 /proc/net/dev 一致。
	m := metricWithLabels(families, "node_network_receive_bytes_total", map[string]string{"device": "eth0"})
	if m == nil {
		t.Fatal("缺少 eth0 的接收字节计数")
	}
	if got := m.GetCounter().GetValue(); got != 1000 {
		t.Errorf("eth0 接收字节 = %v，期望 1000", got)
	}

	// 丢包按原因区分。
	m = metricWithLabels(families, "node_network_receive_dropped_total",
		map[string]string{"device": "eth0", "reason": "total"})
	if m == nil || m.GetCounter().GetValue() != 2 {
		t.Errorf("eth0 接收丢包(total) 指标不正确: %v", m)
	}
	m = metricWithLabels(families, "node_network_transmit_dropped_total",
		map[string]string{"device": "eth0", "reason": "collisions"})
	if m == nil || m.GetCounter().GetValue() != 10 {
		t.Errorf("eth0 发送丢包(collisions) 指标不正确: %v", m)
	}

	// 默认筛选会排除 lo 与 veth1234。
	for _, dev := range []string{"lo", "veth1234"} {
		if m := metricWithLabels(families, "node_network_info", map[string]string{"device": dev}); m != nil {
			t.Errorf("%s 不应被导出（默认排除虚拟网卡）", dev)
		}
	}

	// 链路速率换算：1000 Mbps -> 1e9 bps。
	m = metricWithLabels(families, "node_network_link_speed_bps", map[string]string{"device": "eth0"})
	if m == nil || m.GetGauge().GetValue() != 1e9 {
		t.Errorf("eth0 链路速率指标不正确: %v", m)
	}

	// 全双工 = 1，未知 = 0.5。
	if m := metricWithLabels(families, "node_network_link_duplex", map[string]string{"device": "eth0"}); m == nil ||
		m.GetGauge().GetValue() != 1 {
		t.Errorf("eth0 duplex 指标不正确")
	}
	if m := metricWithLabels(families, "node_network_link_duplex", map[string]string{"device": "wlp1s0"}); m == nil ||
		m.GetGauge().GetValue() != 0.5 {
		t.Errorf("wlp1s0 duplex 应为 0.5（未知）")
	}

	// 虚拟网卡没有链路速率指标。
	if m := metricWithLabels(families, "node_network_link_speed_bps", map[string]string{"device": "veth1234"}); m != nil {
		t.Error("veth1234 不应有链路速率指标（speed=-1 视为未知）")
	}

	// 首次采集没有速率指标。
	if m := metricWithLabels(families, "node_network_receive_bytes_per_second", map[string]string{"device": "eth0"}); m != nil {
		t.Error("首次采集不应输出每秒速率")
	}

	// 导出数量与被过滤数量。
	var exported, filtered float64
	for _, mf := range families {
		switch mf.GetName() {
		case "node_network_interfaces":
			exported = mf.GetMetric()[0].GetGauge().GetValue()
		case "node_network_interfaces_filtered":
			filtered = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if exported != 2 {
		t.Errorf("导出网卡数 = %v，期望 2（lo 与 veth1234 被排除）", exported)
	}
	if filtered != 2 {
		t.Errorf("被过滤网卡数 = %v，期望 2", filtered)
	}
}

// TestCollectRatesNeedTwoScrapes 验证速率在第二次采集后出现且数值正确。
func TestCollectRatesNeedTwoScrapes(t *testing.T) {
	procRoot, sysRoot := makeRoots(t, "Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n")
	filter, _ := util.NewFilter("", "")

	exporter := New(procRoot, sysRoot, filter)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)

	if _, err := reg.Gather(); err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}

	// 接收字节增加 100000，发送增加 50000。
	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(procRoot, "net", "dev"),
		"Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n  eth0: 101000 110 0 0 0 0 0 0 205000 120 0 0 0 0 0 0\n")

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("二次 Gather 失败: %v", err)
	}
	m := metricWithLabels(families, "node_network_receive_bytes_per_second", map[string]string{"device": "eth0"})
	if m == nil {
		t.Fatal("第二次采集应输出每秒速率")
	}
	rate := m.GetGauge().GetValue()
	// 间隔约 10ms，增量 100000 字节，速率应在百万量级；只验证为正且合理。
	if rate <= 0 {
		t.Errorf("接收速率 = %v，期望大于 0", rate)
	}

	m = metricWithLabels(families, "node_network_transmit_packets_per_second", map[string]string{"device": "eth0"})
	if m == nil || m.GetGauge().GetValue() <= 0 {
		t.Errorf("发送包速率不正确: %v", m)
	}
}

// TestLargeCountersNotTruncated 验证超过 int32 范围的累计值不会溢出。
func TestLargeCountersNotTruncated(t *testing.T) {
	procRoot, sysRoot := makeRoots(t, netDevSample)

	// 空 exclude，导出全部网卡。
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(procRoot, sysRoot, nil))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	m := metricWithLabels(families, "node_network_receive_bytes_total", map[string]string{"device": "lo"})
	if m == nil {
		t.Fatal("缺少 lo 的接收字节计数")
	}
	if got := m.GetCounter().GetValue(); got != 70455406765 {
		t.Errorf("lo 接收字节 = %v，期望 70455406765（不能被截断成 int32）", got)
	}
}

// TestCollectMissingNetDev 验证文件缺失时只报错不 panic。
func TestCollectMissingNetDev(t *testing.T) {
	sysRoot := t.TempDir()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(t.TempDir(), sysRoot, nil))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == "node_network_scrape_error" {
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 1 {
				t.Errorf("scrape_error = %v，期望 1", v)
			}
			return
		}
	}
	t.Fatal("缺少 node_network_scrape_error")
}

// TestConcurrentCollectKeepsRates 回归测试：并发抓取时每秒速率不能消失。
//
// 与 CPU 采集器同一个问题：读 /proc/net/dev 与更新基线必须在同一个锁内，
// 否则并发抓取会算出错误的速率甚至整组缺失。
func TestConcurrentCollectKeepsRates(t *testing.T) {
	procRoot, sysRoot := makeRoots(t, "Inter-|   Receive |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n  eth0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0\n")

	exporter := New(procRoot, sysRoot, nil)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)

	if _, err := reg.Gather(); err != nil {
		t.Fatalf("首次 Gather 失败: %v", err)
	}

	const workers = 8
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		bad int
		got int
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			families, err := reg.Gather()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("并发 Gather 失败: %v", err)
				return
			}
			got++
			if metricWithLabels(families, "node_network_receive_bytes_per_second",
				map[string]string{"device": "eth0"}) == nil {
				bad++
			}
		}()
	}
	wg.Wait()

	if got != workers {
		t.Fatalf("只完成了 %d 次采集，期望 %d", got, workers)
	}
	if bad != 0 {
		t.Errorf("有 %d/%d 次并发采集缺少速率指标（基线更新必须与读取同锁）", bad, got)
	}
}
