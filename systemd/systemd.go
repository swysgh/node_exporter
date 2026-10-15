// Package systemd 采集 systemd 服务（unit）状态指标。
//
// 数据来源：
//
//	systemctl list-units          服务名、加载状态、激活状态、子状态
//	systemctl show <unit>...      单个 unit 的内存、CPU 时间、任务数、重启次数
//
// 支持按 unit 名做包含/排除筛选；也可以只关注某几种 unit 类型
// （service / socket / mount / timer ...）。
package systemd

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// DefaultTypes 是默认采集的 unit 类型。
const DefaultTypes = "service"

// showBatchSize 是每次 systemctl show 传入的 unit 数量。
//
// systemctl show 一次接受多个 unit 并顺序输出各自的属性块；
// 分批是为了避免命令行过长（Linux 单条命令的 argv 上限约 2MB）。
const showBatchSize = 32

// states 是 node_systemd_unit_state 使用的一组状态标签。
//
// 采用 one-hot 编码：某个 unit 在某个状态下为 1，其余为 0，
// 缺失即为 0，这样可以直接用 == 0 做告警判断。
var states = []string{
	"active", "activating", "deactivating", "reloading",
	"failed", "inactive",
}

// Unit 保存一个 unit 的 list-units 信息。
type Unit struct {
	Name        string
	Load        string // loaded / not-found / masked ...
	Active      string // active / inactive / failed / activating ...
	Sub         string // running / dead / exited / failed ...
	Description string
}

// listUnitRE 匹配 list-units 的一行数据：前 4 列用连续空白分隔，
// 第 5 列（描述）保留其中的空格。
var listUnitRE = regexp.MustCompile(`^(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s*(.*)$`)

// ParseListUnits 解析 `systemctl list-units` 的输出。
//
// 使用 --no-legend --plain --full 时每行形如（各列用空格对齐，
// 列宽取决于最长的 unit 名，因此不能按单个空格切分）：
//
//	nginx.service     loaded   active   running   Nginx HTTP server
//
// 表头（ACTIVE SUB DESCRIPTION）与以 "●" 开头的告警行会被跳过。
func ParseListUnits(content string) []Unit {
	var units []Unit

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		// 告警行以实心圆点开头，例如 "● foo.service - description"。
		if strings.HasPrefix(strings.TrimSpace(trimmed), "●") {
			continue
		}
		// 表头行。
		if strings.HasPrefix(trimmed, "ACTIVE ") || strings.HasPrefix(trimmed, "UNIT ") {
			continue
		}

		// UNIT LOAD ACTIVE SUB DESCRIPTION...
		// 注意 systemctl 会用空格把各列对齐（列宽随最长 unit 名变化），
		// 因此不能按单个空格切分，必须按"连续空白"切分。
		m := listUnitRE.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		units = append(units, Unit{
			Name:        m[1],
			Load:        m[2],
			Active:      m[3],
			Sub:         m[4],
			Description: strings.TrimSpace(m[5]),
		})
	}

	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units
}

// showProperties 是需要用 systemctl show 采集的属性。
// 顺序与输出解析无关（按 Key=Value 匹配），但保持可读。
var showProperties = []string{
	"Id", "ActiveState", "SubState", "LoadState",
	"MemoryCurrent", "CPUUsageNSec", "TasksCurrent", "NRestarts",
}

// UnitDetail 保存 systemctl show 返回的单个 unit 运行时详情。
type UnitDetail struct {
	Name          string
	ActiveState   string
	SubState      string
	LoadState     string
	MemoryCurrent float64
	CPUUsageNSec  float64
	TasksCurrent  float64
	NRestarts     float64
}

// ParseShowOutput 解析 `systemctl show` 的输出。
//
// 输出形如若干个 "Key=Value" 块，块之间用空行分隔，每块对应一个 unit：
//
//	NRestarts=0
//	MemoryCurrent=10162176
//	CPUUsageNSec=43706975000
//	TasksCurrent=1
//	Id=cron.service
//	LoadState=loaded
//	ActiveState=active
//	SubState=running
//
//	（空行）
//
//	……下一个 unit……
//
// 注意属性顺序由 systemd 内部决定，Id 既不是第一个也不是最后一个，
// 因此这里以"空行分块 + Id 作为块标识"来解析，并且容忍 Id 之前出现的属性
// （先暂存，遇到 Id 时再归入该 unit）。
// 解析结果按 unit 名索引，便于按名字查找。
func ParseShowOutput(content string) map[string]UnitDetail {
	out := make(map[string]UnitDetail)

	var cur UnitDetail
	var haveID bool
	// Id 出现之前读到的属性，暂存后归入当前 unit。
	var pending map[string]string

	flush := func() {
		if haveID && cur.Name != "" {
			out[cur.Name] = cur
		}
		cur = UnitDetail{}
		haveID = false
		pending = nil
	}

	setField := func(d *UnitDetail, key, value string) {
		switch key {
		case "ActiveState":
			d.ActiveState = value
		case "SubState":
			d.SubState = value
		case "LoadState":
			d.LoadState = value
		case "MemoryCurrent":
			d.MemoryCurrent = parseSize(value)
		case "CPUUsageNSec":
			d.CPUUsageNSec = parseDurationSeconds(value)
		case "TasksCurrent":
			d.TasksCurrent = parseSize(value)
		case "NRestarts":
			d.NRestarts = util.ParseFloat64(value)
		}
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}

		if key == "Id" {
			// 出现新的 Id：如果当前块已有 Id，说明没有空行分隔，
			// 需要先把上一块收尾。
			if haveID {
				flush()
			}
			for k, v := range pending {
				setField(&cur, k, v)
			}
			pending = nil
			cur.Name = value
			haveID = true
			continue
		}

		if haveID {
			setField(&cur, key, value)
		} else {
			if pending == nil {
				pending = make(map[string]string, 8)
			}
			pending[key] = value
		}
	}
	flush()
	return out
}

// parseSize 解析 systemctl 的尺寸字段。
// 输出可能是 "12582912" 或 "[not set]" 或 "[infinity]"，后两者返回 0。
func parseSize(s string) float64 {
	if strings.HasPrefix(s, "[") {
		return 0
	}
	v, ok := util.ParseNumber(s)
	if !ok {
		return 0
	}
	return v
}

// parseDurationSeconds 解析 systemctl 的时长字段，结果统一为秒。
//
// 支持以下写法（结果统一为秒）：
//
//	"43706975000"   裸数字 → 纳秒（systemd 的 *_NSec 属性就是这样输出的）
//	"1s 234567890"  复合时长 → 各段相加
//	"500ms"/"2min"  带单位 → 换算成秒
//	"[not set]"     返回 0
func parseDurationSeconds(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return 0
	}
	if strings.ContainsAny(s, " ") {
		total := 0.0
		for _, part := range strings.Fields(s) {
			total += parseDurationSeconds(part)
		}
		return total
	}

	// 从左侧切分出数值部分与单位部分：
	// 数值可能是 "500"、"1.5"、"1e9"，单位可能是 "ms"、"min"、"s" 或空。
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		if (c == 'e' || c == 'E') && end > 0 && end+1 < len(s) &&
			((s[end+1] >= '0' && s[end+1] <= '9') || s[end+1] == '-' || s[end+1] == '+') {
			end++
			continue
		}
		break
	}
	v, ok := util.ParseNumber(s[:end])
	if !ok {
		return 0
	}

	switch strings.TrimSpace(s[end:]) {
	case "", "ns":
		// 裸数字按纳秒处理：本函数只用于 *_NSec 结尾的 systemd 属性，
		// 这些属性在不带单位时输出的就是纳秒计数。
		return v / 1e9
	case "us", "µs":
		return v / 1e6
	case "ms":
		return v / 1e3
	case "s":
		return v
	case "min", "m":
		return v * 60
	case "h":
		return v * 3600
	case "d":
		return v * 86400
	}
	return v
}

// Exporter 是 systemd unit 指标的 Prometheus Collector。
type Exporter struct {
	bin        string
	filter     *util.Filter
	types      []string
	withDetail bool
	timeout    time.Duration

	unitState    *prometheus.Desc
	unitInfo     *prometheus.Desc
	unitMemory   *prometheus.Desc
	unitCPU      *prometheus.Desc
	unitTasks    *prometheus.Desc
	unitRestarts *prometheus.Desc

	unitsTotal     *prometheus.Desc
	unitsRunning   *prometheus.Desc
	unitsFailed    *prometheus.Desc
	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// Options 是 systemd Collector 的配置项。
type Options struct {
	// Bin 是 systemctl 的路径，默认 "systemctl"。
	Bin string
	// Filter 是 unit 名筛选规则，为 nil 时不过滤。
	Filter *util.Filter
	// Types 是要采集的 unit 类型（service/socket/mount/timer...），为空则用默认 service。
	Types []string
	// WithDetail 为 true 时额外调用 systemctl show 采集内存/CPU/重启次数。
	WithDetail bool
	// Timeout 是单次 systemctl 调用的超时。
	Timeout time.Duration
}

// New 使用给定配置创建 systemd Collector。
func New(opts Options) *Exporter {
	if opts.Bin == "" {
		opts.Bin = "systemctl"
	}
	if len(opts.Types) == 0 {
		opts.Types = []string{DefaultTypes}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	// 去掉空白项（例如 "service,,socket"）。
	types := make([]string, 0, len(opts.Types))
	for _, t := range opts.Types {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	opts.Types = types

	return &Exporter{
		bin:        opts.Bin,
		filter:     opts.Filter,
		types:      opts.Types,
		withDetail: opts.WithDetail,
		timeout:    opts.Timeout,

		unitState: prometheus.NewDesc(
			namespace+"_systemd_unit_state",
			"systemd unit 状态（one-hot：1 表示处于该状态，其余为 0）",
			[]string{"unit", "state"}, nil,
		),
		unitInfo: prometheus.NewDesc(
			namespace+"_systemd_unit_info",
			"systemd unit 静态信息，值恒为 1",
			[]string{"unit", "load_state", "active_state", "sub_state", "description"}, nil,
		),
		unitMemory: prometheus.NewDesc(
			namespace+"_systemd_unit_memory_bytes",
			"unit 当前占用的内存（MemoryCurrent）", []string{"unit"}, nil,
		),
		unitCPU: prometheus.NewDesc(
			namespace+"_systemd_unit_cpu_seconds_total",
			"unit 累计使用的 CPU 时间（秒）", []string{"unit"}, nil,
		),
		unitTasks: prometheus.NewDesc(
			namespace+"_systemd_unit_tasks",
			"unit 当前的任务（进程/线程）数", []string{"unit"}, nil,
		),
		unitRestarts: prometheus.NewDesc(
			namespace+"_systemd_unit_restarts_total",
			"unit 自动重启次数", []string{"unit"}, nil,
		),

		unitsTotal: prometheus.NewDesc(
			namespace+"_systemd_units",
			"被采集的 unit 总数", []string{"type"}, nil,
		),
		unitsRunning: prometheus.NewDesc(
			namespace+"_systemd_units_running",
			"处于 running 子状态的 unit 数量", []string{"type"}, nil,
		),
		unitsFailed: prometheus.NewDesc(
			namespace+"_systemd_units_failed",
			"处于 failed 状态的 unit 数量", []string{"type"}, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_systemd_scrape_duration_seconds", "systemd 指标采集耗时（秒）", nil, nil,
		),
		scrapeError: prometheus.NewDesc(
			namespace+"_systemd_scrape_error", "systemd 指标采集是否出错（1=出错, 0=正常）", nil, nil,
		),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.unitState
	ch <- e.unitInfo
	ch <- e.unitMemory
	ch <- e.unitCPU
	ch <- e.unitTasks
	ch <- e.unitRestarts
	ch <- e.unitsTotal
	ch <- e.unitsRunning
	ch <- e.unitsFailed
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
//
// systemctl 不可用（非 systemd 系统、容器内缺少 dbus 等）时不会让整个
// 抓取失败，只会把 node_systemd_scrape_error 置为 1 并记录日志。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	// ---- 列出所有 unit ----
	args := []string{
		"list-units", "--all", "--no-legend", "--plain", "--no-pager", "--full",
		"--type=" + strings.Join(e.types, ","),
	}
	res := util.RunCommand(ctx, e.bin, args...)
	if res.Err != nil && strings.TrimSpace(res.Stdout) == "" {
		log.Printf("systemd 采集失败（%s）: %v", strings.Join(args, " "), res.Err)
		e.reportErrors(ch, start, 1, 0, 0, 0, 0)
		return
	}

	all := ParseListUnits(res.Stdout)

	// ---- 按筛选规则过滤 ----
	var units []Unit
	filteredOut := 0
	for _, u := range all {
		if !e.filter.Match(u.Name) {
			filteredOut++
			continue
		}
		units = append(units, u)
	}

	// ---- 逐个 unit 的运行时详情 ----
	details := map[string]UnitDetail{}
	if e.withDetail && len(units) > 0 {
		details = e.scrapeDetails(ctx, units)
		if len(details) == 0 {
			// show 失败不影响其余指标（状态信息仍然有效）。
			log.Printf("systemctl show 未返回任何数据，内存/CPU/重启次数指标将被跳过")
		}
	}

	// ---- 输出指标 ----
	running, failed := 0, 0
	for _, u := range units {
		// one-hot 状态：对每个已知状态输出 0/1。
		for _, st := range states {
			v := 0.0
			if u.Active == st {
				v = 1.0
			}
			ch <- prometheus.MustNewConstMetric(e.unitState, prometheus.GaugeValue, v, u.Name, st)
		}
		if u.Active == "failed" {
			failed++
		}
		if u.Sub == "running" {
			running++
		}

		ch <- prometheus.MustNewConstMetric(e.unitInfo, prometheus.GaugeValue, 1,
			u.Name, u.Load, u.Active, u.Sub, u.Description)

		if d, ok := details[u.Name]; ok {
			ch <- prometheus.MustNewConstMetric(e.unitMemory, prometheus.GaugeValue, d.MemoryCurrent, u.Name)
			ch <- prometheus.MustNewConstMetric(e.unitCPU, prometheus.CounterValue, d.CPUUsageNSec, u.Name)
			ch <- prometheus.MustNewConstMetric(e.unitTasks, prometheus.GaugeValue, d.TasksCurrent, u.Name)
			ch <- prometheus.MustNewConstMetric(e.unitRestarts, prometheus.CounterValue, d.NRestarts, u.Name)
		}
	}

	e.reportErrors(ch, start, 0, len(units), filteredOut, running, failed)
}

// scrapeDetails 分批调用 systemctl show 采集 unit 详情。
func (e *Exporter) scrapeDetails(ctx context.Context, units []Unit) map[string]UnitDetail {
	out := make(map[string]UnitDetail, len(units))

	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Name)
	}

	for start := 0; start < len(names); start += showBatchSize {
		end := start + showBatchSize
		if end > len(names) {
			end = len(names)
		}
		batch := names[start:end]

		args := append([]string{"show", "--no-pager"}, showPropertyArgs()...)
		args = append(args, batch...)

		res := util.RunCommand(ctx, e.bin, args...)
		if res.Err != nil {
			log.Printf("systemctl show 失败（%d 个 unit）: %v", len(batch), res.Err)
			continue
		}
		for name, d := range ParseShowOutput(res.Stdout) {
			out[name] = d
		}
	}
	return out
}

// showPropertyArgs 返回 systemctl show 的 --property 参数。
func showPropertyArgs() []string {
	args := make([]string, 0, len(showProperties)*2)
	for _, p := range showProperties {
		args = append(args, "--property="+p)
	}
	return args
}

// reportErrors 输出采集耗时、错误标记与 unit 计数。
func (e *Exporter) reportErrors(ch chan<- prometheus.Metric, start time.Time, errValue float64, total, filtered, running, failed int) {
	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
	for _, t := range e.types {
		ch <- prometheus.MustNewConstMetric(e.unitsTotal, prometheus.GaugeValue, float64(total), t)
		ch <- prometheus.MustNewConstMetric(e.unitsRunning, prometheus.GaugeValue, float64(running), t)
		ch <- prometheus.MustNewConstMetric(e.unitsFailed, prometheus.GaugeValue, float64(failed), t)
	}
	ch <- prometheus.MustNewConstMetric(e.unitsTotal, prometheus.GaugeValue, float64(filtered), "filtered")
}

// String 返回采集范围的描述，用于启动日志。
func (e *Exporter) String() string {
	detail := "关"
	if e.withDetail {
		detail = "开"
	}
	return fmt.Sprintf("types=%s filter(%s) detail=%s", strings.Join(e.types, ","), e.filter, detail)
}
