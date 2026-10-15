// Package smart 采集硬盘 S.M.A.R.T. 指标（健康状态、温度、寿命、错误计数等）。
//
// 采集方式：调用 smartctl 解析其文本输出，因此需要安装 smartmontools，
// 并且 exporter 进程需要能读取 /dev/sd*、/dev/nvme* 等设备
// （通常意味着需要 root 权限，或加入 disk 组并配置 udev 规则）。
//
// 为避免唤醒待机硬盘，默认带 -n standby 参数：硬盘处于待机时
// 只记录"处于待机"状态而不读取 SMART 数据。
package smart

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// DefaultExcludeRegex 是默认排除的设备。
//
// 这些设备类型没有有意义的 SMART 数据（zram/loop 是内存盘，
// sr 是光驱，dm- 是 device mapper，nbd 是网络块设备），
// 对它们执行 smartctl 只会产生无用的错误日志。
const DefaultExcludeRegex = `^/dev/(zram|loop|sr|fd|dm-|md|nbd|zram)`

// Attribute 是一个 ATA S.M.A.R.T. 属性。
type Attribute struct {
	ID    int
	Name  string
	Value float64 // RAW_VALUE：最具诊断意义的原始值
	// Normalized 是归一化后的 VALUE（0-100），部分磁盘没有该字段。
	Normalized *float64
}

// Info 保存一块磁盘的 S.M.A.R.T. 摘要。
type Info struct {
	Device       string
	Model        string
	Serial       string
	Interface    string // ata / sat / scsi / nvme / unknown
	Health       float64
	HealthOK     bool
	Standby      bool
	Temperature  *float64
	Rotational   *float64
	PowerOnHours *float64
	Attributes   []Attribute
}

// attrRE 匹配 ATA 属性表的数据行：
//
//	ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
//	  5 Reallocated_Sector_Ct  0x0033   100   100   010    Pre-fail  Always       -       0
//	194 Temperature_Celsius     0x0022   032   060   000    Old_age   Always       -       32 (Min/Max 20/45)
var attrRE = regexp.MustCompile(
	`^\s*(\d+)\s+(\S+)\s+(\S+)\s+(\d+)\s+(\d+)\s+(\d+)\s+\S+\s+\S+\s+\S+\s+(\d+)`)

// ParseInfo 解析 `smartctl -a [-n standby] <device>` 的输出。
//
// 兼容 ATA/SATA（"SMART overall-health self-assessment test result: PASSED"）、
// SCSI（"SMART Health Status: OK"）以及 NVMe（"SMART/Health Information (NVMe Log 0x02)"）
// 三种格式。无法解析出健康状态时 HealthOK 为 false，调用方应据此报警。
func ParseInfo(device, content string) Info {
	info := Info{Device: device, Interface: "unknown"}
	var vendor string

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)

		switch {
		// ---- 待机 / 休眠：本次没有真正读取 SMART ----
		case containsAny(trimmed, "STANDBY", "SLEEP") && containsAny(trimmed, "Device is in"):
			info.Standby = true
			continue

		// ---- 健康状态 ----
		case contains(trimmed, "SMART overall-health self-assessment test result:"):
			v := valueAfter(trimmed, ":")
			info.HealthOK = strings.EqualFold(v, "PASSED")
			info.Health = boolToFloat(info.HealthOK)
			continue
		case contains(trimmed, "SMART Health Status:"):
			v := valueAfter(trimmed, ":")
			info.HealthOK = strings.EqualFold(strings.TrimSpace(v), "OK")
			info.Health = boolToFloat(info.HealthOK)
			continue

		// ---- 型号 / 序列号 ----
		case hasPrefix(trimmed, "Device Model:"):
			info.Model = valueAfter(trimmed, ":")
		case hasPrefix(trimmed, "Model Number:"):
			if info.Model == "" {
				info.Model = valueAfter(trimmed, ":")
			}
		case hasPrefix(trimmed, "Serial Number:"):
			info.Serial = valueAfter(trimmed, ":")
		case hasPrefix(trimmed, "Vendor:"):
			// SCSI/SAS 输出把厂商与型号分成两行，这里先记下厂商，
			// 等 Product 行出现时拼成完整型号。
			vendor = valueAfter(trimmed, ":")
			if info.Model == "" {
				info.Model = vendor
			}
		case hasPrefix(trimmed, "Product:"):
			model := valueAfter(trimmed, ":")
			if vendor != "" {
				info.Model = strings.TrimSpace(vendor + " " + model)
			} else {
				info.Model = model
			}

		// ---- 接口类型 ----
		case hasPrefix(trimmed, "ATA Version is:"):
			info.Interface = "ata"
		case hasPrefix(trimmed, "SATA Version is:"):
			// SATA 比 ATA 更具体：SATA 设备同时会打印 "ATA Version is:"，
			// 因此这里直接覆盖为 sat。
			info.Interface = "sat"
		case hasPrefix(trimmed, "NVMe Version is:"):
			info.Interface = "nvme"
		case contains(trimmed, "NVMe Log 0x02"):
			info.Interface = "nvme"
		case hasPrefix(trimmed, "Transport protocol:"):
			if strings.Contains(trimmed, "NVMe") {
				info.Interface = "nvme"
			} else if info.Interface == "unknown" {
				info.Interface = "sat"
			}

		// ---- 温度：NVMe 与 SAS/SCSI 的独立字段 ----
		case hasPrefix(trimmed, "Temperature:"):
			// 形如 "Temperature:                        41 Celsius"
			if n, ok := parseCelsiusValue(trimmed); ok {
				info.Temperature = &n
			}
		case hasPrefix(trimmed, "Current Drive Temperature:"):
			if n, ok := parseCelsiusValue(trimmed); ok {
				info.Temperature = &n
			}
		case hasPrefix(trimmed, "Temperature_Celsius"):
			// NVMe 风格："Temperature_Celsius: 41"，也可能带 SMART 属性表格式。
			if n, ok := parseCelsiusValue(trimmed); ok {
				info.Temperature = &n
			}

		// ---- 旋转状态 ----
		case hasPrefix(trimmed, "Rotation Rate:"):
			v := strings.ToLower(valueAfter(trimmed, ":"))
			switch {
			case strings.Contains(v, "solid state"):
				info.Rotational = boolToFloatPtr(false)
			case strings.Contains(v, "rpm"):
				info.Rotational = boolToFloatPtr(true)
			}

		// ---- 通电时间（NVMe/SCSI 独立字段，ATA 走属性 9）----
		case hasPrefix(trimmed, "Power On Hours:"), hasPrefix(trimmed, "Power-on Hours:"):
			if v, ok := parseSmartNumber(valueAfter(trimmed, ":")); ok {
				info.PowerOnHours = &v
			}
		case hasPrefix(trimmed, "Power Cycles:"), hasPrefix(trimmed, "Power Cycle Count:"):
			// 属于可用属性，这里不做特殊处理（属性表已覆盖 ATA 设备）。
		}

		// ---- ATA 属性表 ----
		if m := attrRE.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			raw, _ := parseSmartNumber(m[7])
			norm, _ := parseSmartNumber(m[4])
			a := Attribute{ID: id, Name: m[2], Value: raw}
			a.Normalized = &norm
			info.Attributes = append(info.Attributes, a)

			// ATA 的温度属性编号在不同厂商间不统一，
			// 这里同时按 ID 与属性名匹配，避免把寿命值当成温度。
			switch {
			case id == 194 && strings.Contains(strings.ToLower(a.Name), "temp"):
				info.setTemperature(raw)
			case id == 190 && strings.Contains(strings.ToLower(a.Name), "temp"):
				info.setTemperature(raw)
			case strings.EqualFold(a.Name, "Temperature_Celsius"):
				info.setTemperature(raw)
			}
			if id == 9 && info.PowerOnHours == nil {
				h := raw
				info.PowerOnHours = &h
			}
		}
	}

	return info
}

// setTemperature 记录温度（摄氏度）。
func (i *Info) setTemperature(v float64) {
	if v == 0 {
		return
	}
	i.Temperature = &v
}

// Attr 返回指定 ID 的属性值；不存在时 ok=false。
func (i Info) Attr(id int) (float64, bool) {
	for _, a := range i.Attributes {
		if a.ID == id {
			return a.Value, true
		}
	}
	return 0, false
}

// AttrByName 按属性名（不区分大小写）查找。
func (i Info) AttrByName(name string) (float64, bool) {
	for _, a := range i.Attributes {
		if strings.EqualFold(a.Name, name) {
			return a.Value, true
		}
	}
	return 0, false
}

// ParseScanDevices 解析 `smartctl --scan` 的输出，返回设备路径列表。
//
// 输出形如：
//
//	/dev/sda -d sat # /dev/sda [SAT], ATA device
//	/dev/nvme0 -d nvme # /dev/nvme0, NVMe device
func ParseScanDevices(content string) []string {
	var devices []string
	seen := make(map[string]bool)

	for _, line := range util.SplitLines(content) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		device := fields[0]
		// 只接受真实块设备路径。
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		if seen[device] {
			continue
		}
		seen[device] = true
		devices = append(devices, device)
	}
	sort.Strings(devices)
	return devices
}

// contains 是 strings.Contains 的简写。
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// containsAny 判断 s 是否包含任意一个子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// hasPrefix 判断 s 是否以 prefix 开头（忽略大小写）。
func hasPrefix(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}

// valueAfter 返回最后一个冒号之后的内容并去除空白。
func valueAfter(s, sep string) string {
	_, v, ok := strings.Cut(s, sep)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// boolToFloat 把布尔值转成 1/0。
func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// boolToFloatPtr 返回指向 1/0 的指针，用于可选指标。
func boolToFloatPtr(b bool) *float64 {
	v := boolToFloat(b)
	return &v
}

// parseSmartNumber 解析 smartctl 输出中的数值。
//
// smartctl 习惯用千分位逗号分隔大数字（例如 "1,000,204,886,016"、"5,678"），
// 直接用 strconv 会失败，因此先去掉逗号再解析。
func parseSmartNumber(s string) (float64, bool) {
	return util.ParseNumber(strings.ReplaceAll(strings.TrimSpace(s), ",", ""))
}

// parseCelsiusValue 解析温度字段中的摄氏度数值。
//
// 兼容 "41 Celsius"、"41 C"、"41" 与 smartctl 的电弧格式（原始温度可能
// 是 41 或 0x29，后者代表 0x29 = 41）。返回值 ok=false 表示无法解析。
func parseCelsiusValue(s string) (float64, bool) {
	_, after, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	// 取第一个数值 token。
	for _, token := range strings.Fields(after) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(token), "0x") {
			if v, err := strconv.ParseUint(token[2:], 16, 64); err == nil {
				return float64(v), true
			}
			continue
		}
		if v, ok := parseSmartNumber(token); ok {
			return v, true
		}
	}
	return 0, false
}

// Options 是 SMART Collector 的配置项。
type Options struct {
	// Bin 是 smartctl 路径，默认 "smartctl"。
	Bin string
	// Devices 是显式指定的设备列表；为空时用 smartctl --scan 自动发现。
	Devices []string
	// Filter 用于排除设备（nil 表示不排除）。
	Filter *util.Filter
	// NoStandby 为 true 时传入 -n standby，硬盘待机时不唤醒它（推荐）。
	// 该值由命令行 -smart.standby 直接决定，New 不会改写它。
	NoStandby bool
	// Parallel 是并发执行 smartctl 的最大数量。
	Parallel int
	// Timeout 是单块硬盘的超时时间。
	Timeout time.Duration
}

// Exporter 是 S.M.A.R.T. 指标的 Prometheus Collector。
type Exporter struct {
	opts Options
}

// New 使用给定配置创建 SMART Collector。
func New(opts Options) *Exporter {
	if opts.Bin == "" {
		opts.Bin = "smartctl"
	}
	if opts.Parallel <= 0 {
		opts.Parallel = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	return &Exporter{opts: opts}
}

// Collect 实现 prometheus.Collector。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	var errs []string

	devices := e.opts.Devices
	if len(devices) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), e.opts.Timeout)
		res := util.RunCommand(ctx, e.opts.Bin, "--scan")
		cancel()
		if res.Err != nil && strings.TrimSpace(res.Stdout) == "" {
			log.Printf("smartctl --scan 失败: %v", res.Err)
			ch <- prometheus.MustNewConstMetric(D.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
			ch <- prometheus.MustNewConstMetric(D.scrapeError, prometheus.GaugeValue, 1)
			return
		}
		devices = ParseScanDevices(res.Stdout)
	}

	// 应用排除规则。
	var targets []string
	for _, d := range devices {
		if !e.opts.Filter.Match(d) {
			continue
		}
		targets = append(targets, d)
	}

	// 并发采集，避免几十块盘时串行等待。
	var (
		mu    sync.Mutex
		infos []Info
	)
	sem := make(chan struct{}, e.opts.Parallel)
	var wg sync.WaitGroup

	for _, device := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(dev string) {
			defer wg.Done()
			defer func() { <-sem }()

			info, err := e.scrapeDevice(dev)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", dev, err))
			}
			if info.Device != "" {
				infos = append(infos, info)
			}
		}(device)
	}
	wg.Wait()

	for _, info := range infos {
		e.emit(ch, info)
	}

	errValue := 0.0
	if len(errs) > 0 {
		errValue = 1.0
		log.Printf("S.M.A.R.T. 采集部分失败: %s", strings.Join(errs, "; "))
	}
	ch <- prometheus.MustNewConstMetric(D.devicesExported, prometheus.GaugeValue, float64(len(infos)))
	ch <- prometheus.MustNewConstMetric(D.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(D.scrapeError, prometheus.GaugeValue, errValue)
}

// scrapeDevice 采集单块硬盘的 SMART 信息。
func (e *Exporter) scrapeDevice(device string) (Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.opts.Timeout)
	defer cancel()

	args := []string{"-a"}
	if e.opts.NoStandby {
		args = append(args, "-n", "standby")
	}
	args = append(args, device)

	res := util.RunCommand(ctx, e.opts.Bin, args...)
	info := ParseInfo(device, res.Stdout+"\n"+res.Stderr)

	if res.Err != nil {
		// 设备待机（smartctl 退出码 2 的 bit1）不是错误。
		if info.Standby {
			return info, nil
		}
		if strings.TrimSpace(res.Stdout) == "" && strings.TrimSpace(res.Stderr) == "" {
			return Info{}, res.Err
		}
		// 有输出说明至少拿到了部分信息（例如 USB 桥不支持 SMART），
		// 仍然导出已解析到的指标，只把失败计入 scrape_error。
		return info, res.Err
	}
	return info, nil
}

// emit 输出单块硬盘的指标。
func (e *Exporter) emit(ch chan<- prometheus.Metric, info Info) {
	device, model, serial := info.Device, info.Model, info.Serial

	ch <- prometheus.MustNewConstMetric(D.up, prometheus.GaugeValue, 1, device, model, serial, info.Interface)
	ch <- prometheus.MustNewConstMetric(D.health, prometheus.GaugeValue, info.Health, device, model, serial)
	ch <- prometheus.MustNewConstMetric(D.standby, prometheus.GaugeValue, boolToFloat(info.Standby), device, model, serial)

	if info.Temperature != nil {
		ch <- prometheus.MustNewConstMetric(D.temperature, prometheus.GaugeValue, *info.Temperature, device, model, serial)
	}
	if info.Rotational != nil {
		ch <- prometheus.MustNewConstMetric(D.rotational, prometheus.GaugeValue, *info.Rotational, device, model, serial)
	}
	if info.PowerOnHours != nil {
		ch <- prometheus.MustNewConstMetric(D.powerOnHours, prometheus.CounterValue, *info.PowerOnHours, device, model, serial)
	}

	// 常用 ATA 错误/寿命属性，单独命名方便告警。
	// 编号取自 ATA/ACS 规范与厂商惯例，不同型号可能缺失，缺失即不输出。
	for _, item := range []struct {
		desc *prometheus.Desc
		ids  []int
	}{
		{D.reallocatedSectors, []int{5}},
		{D.powerCycles, []int{12}},
		{D.powerOnHoursAttr, []int{9}},
		{D.reallocatedEvents, []int{196}},
		{D.currentPendingSectors, []int{197}},
		{D.offlineUncorrectable, []int{198}},
		{D.crcErrors, []int{199}},
		{D.mediaWearout, []int{233}},
	} {
		if v, ok := info.Attr(item.ids[0]); ok {
			ch <- prometheus.MustNewConstMetric(item.desc, prometheus.CounterValue, v, device, model, serial)
		}
	}

	// 部分厂商把 SSD 寿命放在 231（名字不统一），这里按名字匹配。
	for _, name := range []struct {
		desc *prometheus.Desc
		key  string
	}{
		{D.mediaWearout, "Media_Wearout_Indicator"},
		{D.mediaWearout, "SSD_Life_Left"},
		{D.mediaWearout, "Wear_Leveling_Count"},
		{D.percentageUsed, "Percentage_Used"},
		{D.availableSpare, "Available_Spare"},
		{D.dataUnitsWritten, "Data_Units_Written"},
		{D.mediaErrors, "Media_and_Data_Integrity_Errors"},
		{D.unsafeShutdowns, "Unsafe_Shutdowns"},
	} {
		if v, ok := info.AttrByName(name.key); ok {
			ch <- prometheus.MustNewConstMetric(name.desc, prometheus.GaugeValue, v, device, model, serial)
		}
	}

	// 全部属性的原始值，便于自定义面板。
	for _, a := range info.Attributes {
		ch <- prometheus.MustNewConstMetric(
			D.attribute, prometheus.GaugeValue, a.Value,
			device, model, serial, strconv.Itoa(a.ID), a.Name,
		)
	}
}

// String 返回采集配置的可读描述，用于启动日志。
func (e *Exporter) String() string {
	devices := "自动发现"
	if len(e.opts.Devices) > 0 {
		devices = strings.Join(e.opts.Devices, ",")
	}
	standby := "唤醒"
	if e.opts.NoStandby {
		standby = "不唤醒"
	}
	return fmt.Sprintf("smartctl=%s devices=%s standby=%s parallel=%d filter(%s)",
		e.opts.Bin, devices, standby, e.opts.Parallel, e.opts.Filter)
}
