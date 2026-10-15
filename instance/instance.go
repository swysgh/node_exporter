// Package instance 负责识别本机 hostname，并在 /metrics 输出时
// 把所有指标的 instance 标签统一改写为该 hostname。
//
// 为什么需要它：Prometheus 默认用抓取目标的 "地址:端口" 作为 instance
// 标签，而 Prometheus 抓取端在 honor_labels=false（默认值）时会让
// exporter 主动上报的标签覆盖掉目标地址。于是 exporter 只要显式输出
// instance="<hostname>"，Grafana/Prometheus 中的实例名就是主机名，
// 而不是 IP:9100。
package instance

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// LabelName 是被改写的标签名。
const LabelName = "instance"

// Detect 识别用于 instance 标签的 hostname，按以下优先级取值：
//
//  1. 命令行显式指定的 override（-hostname）
//  2. os.Hostname()（等价于 uname -n 的实现）
//  3. /etc/hostname 的第一行
//  4. `hostname` 命令的输出
//  5. `uname -n` 的输出
//  6. 兜底常量 "unknown"
//
// 返回值已经过清洗：去除首尾空白、去掉常见的 FQDN 尾点，
// 并保证不以 "localhost" 结尾之外的无意义内容结尾。
func Detect(override string) string {
	if s := clean(override); s != "" {
		return s
	}

	// os.Hostname 内部走 uname(2)，是绝大多数 Linux 系统上最可靠的方式。
	if h, err := os.Hostname(); err == nil {
		if s := clean(h); s != "" {
			return s
		}
	}

	// 回退 1：/etc/hostname（Debian/RHEL 系都有）。
	if s := firstLine("/etc/hostname"); s != "" {
		return s
	}

	// 回退 2：外部命令（容器/精简系统里 os.Hostname 可能被屏蔽）。
	for _, cmd := range [][]string{{"hostname"}, {"uname", "-n"}} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := exec.Command(cmd[0], cmd[1:]...).Output()
		if err != nil {
			continue
		}
		if s := clean(string(out)); s != "" {
			return s
		}
	}

	return "unknown"
}

// firstLine 读取文件的第一行并清洗，文件不存在或为空时返回 ""。
func firstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		return clean(scanner.Text())
	}
	return ""
}

// clean 清洗 hostname：去空白、去 FQDN 尾点、压缩内部多余空白。
func clean(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 只取第一行，避免命令输出里混入换行。
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// 去掉 FQDN 末尾的根点，例如 "host.example.com." -> "host.example.com"
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return ""
	}
	// 剔除明显不是主机名的占位值。
	switch strings.ToLower(s) {
	case "localhost", "localhost.localdomain", "(none)", "unknown":
		return ""
	}
	return s
}

// Gatherer 是一个 prometheus.Gatherer 包装器。
// 它在内层 Gatherer 完成采集后，遍历所有指标，
// 把 instance 标签的值改写为指定 hostname；缺失时补上。
type Gatherer struct {
	inner    prometheus.Gatherer
	hostname string
}

// WrapGatherer 用 instance 标签改写包装内层 Gatherer。
func WrapGatherer(inner prometheus.Gatherer, hostname string) prometheus.Gatherer {
	return &Gatherer{inner: inner, hostname: hostname}
}

// Gather 实现 prometheus.Gatherer。
//
// 改写策略是"替换"而非"追加"：
//   - 已存在 instance 标签：直接改写其值
//   - 不存在 instance 标签：新增一个（并维持标签按名称有序）
//
// 这样即使某个采集器自行输出了 instance 标签，也不会出现重复标签，
// 而是被统一收敛为本机 hostname。
func (g *Gatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.inner.Gather()

	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			g.apply(m)
		}
	}

	return families, err
}

// apply 把单个指标的 instance 标签设置为 g.hostname。
func (g *Gatherer) apply(m *dto.Metric) {
	if m == nil {
		return
	}

	labels := m.GetLabel()
	for _, lp := range labels {
		if lp.GetName() == LabelName {
			v := g.hostname
			lp.Value = &v
			return
		}
	}

	// 标签不存在：补一个，并按名称排序保持输出稳定。
	v := g.hostname
	labels = append(labels, &dto.LabelPair{
		Name:  strPtr(LabelName),
		Value: &v,
	})
	sort.SliceStable(labels, func(i, j int) bool {
		return labels[i].GetName() < labels[j].GetName()
	})
	m.Label = labels
}

// strPtr 返回指向 s 的指针，用于构造 protobuf 的可选字符串字段。
func strPtr(s string) *string { return &s }

// String 返回包装器的可读描述，便于调试输出。
func (g *Gatherer) String() string {
	return fmt.Sprintf("instanceGatherer{hostname=%q}", g.hostname)
}
