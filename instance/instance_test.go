package instance

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeGatherer 返回预置指标族，用于测试标签改写。
type fakeGatherer struct {
	families []*dto.MetricFamily
}

func (f *fakeGatherer) Gather() ([]*dto.MetricFamily, error) { return f.families, nil }

// metricPair 构造一个带 label 的指标族。
func metricPair(name string, labels map[string]string) *dto.MetricFamily {
	v := 1.0
	pairs := make([]*dto.LabelPair, 0, len(labels))
	for k, val := range labels {
		vv := val
		kk := k
		pairs = append(pairs, &dto.LabelPair{Name: &kk, Value: &vv})
	}
	return &dto.MetricFamily{
		Name: proto(name),
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{{
			Label: pairs,
			Gauge: &dto.Gauge{Value: &v},
		}},
	}
}

func proto(s string) *string { return &s }

// TestGathererAddsInstance 验证缺失 instance 标签时会被补上。
func TestGathererAddsInstance(t *testing.T) {
	inner := &fakeGatherer{families: []*dto.MetricFamily{
		metricPair("no_labels", nil),
		metricPair("other_labels", map[string]string{"device": "eth0"}),
	}}

	families, err := WrapGatherer(inner, "myhost").Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			found := false
			for _, lp := range labels {
				if lp.GetName() == "instance" && lp.GetValue() == "myhost" {
					found = true
				}
			}
			if !found {
				t.Errorf("指标 %s 缺少 instance=\"myhost\"，标签为 %v", mf.GetName(), labels)
			}
		}
	}
}

// TestGathererReplacesInstance 验证已有的 instance 标签会被改写而不是重复添加。
func TestGathererReplacesInstance(t *testing.T) {
	inner := &fakeGatherer{families: []*dto.MetricFamily{
		metricPair("already_has_instance", map[string]string{
			"instance": "10.0.0.5:9100",
			"device":   "eth0",
		}),
	}}

	families, _ := WrapGatherer(inner, "myhost").Gather()
	labels := families[0].GetMetric()[0].GetLabel()

	count := 0
	for _, lp := range labels {
		if lp.GetName() == "instance" {
			count++
			if lp.GetValue() != "myhost" {
				t.Errorf("instance = %q，期望被改写为 myhost", lp.GetValue())
			}
		}
	}
	if count != 1 {
		t.Errorf("instance 标签出现了 %d 次，期望 1 次（不能重复）", count)
	}
}

// TestGathererKeepsLabelOrder 验证补标签后标签保持有序。
func TestGathererKeepsLabelOrder(t *testing.T) {
	inner := &fakeGatherer{families: []*dto.MetricFamily{
		metricPair("sorted", map[string]string{"zzz": "1", "aaa": "2"}),
	}}

	families, _ := WrapGatherer(inner, "h").Gather()
	labels := families[0].GetMetric()[0].GetLabel()

	var names []string
	for _, lp := range labels {
		names = append(names, lp.GetName())
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("标签未按名称排序: %v", names)
		}
	}
}

// TestDetectOverride 验证 -hostname 优先级最高。
func TestDetectOverride(t *testing.T) {
	cases := map[string]string{
		"explicit.example.com": "explicit.example.com",
		"  spaced  ":           "spaced",
		"trailing.dot.":        "trailing.dot",
		"localhost":            "unknown", // 占位值会被清洗掉，回落到自动识别
	}
	for in, want := range cases {
		if in == "localhost" {
			// 该机器的自动识别结果不固定，只验证它不是 "localhost"。
			got := Detect(in)
			if got == "localhost" {
				t.Error("Detect(localhost) 不应返回 localhost")
			}
			continue
		}
		if got := Detect(in); got != want {
			t.Errorf("Detect(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDetectAuto 验证自动识别能返回非空且合法的 hostname。
func TestDetectAuto(t *testing.T) {
	got := Detect("")
	if got == "" {
		t.Fatal("Detect(\"\") 不应返回空字符串")
	}
	if strings.ContainsAny(got, " \t\n") {
		t.Errorf("Detect(\"\") = %q，包含空白字符", got)
	}
	if got != "unknown" {
		t.Logf("自动识别的 hostname: %s", got)
	}
}

// TestClean 验证 hostname 清洗逻辑。
func TestClean(t *testing.T) {
	cases := map[string]string{
		"host.example.com.": "host.example.com",
		"  host  ":          "host",
		"host\ninjected":    "host",
		"":                  "",
		"localhost":         "",
		"LOCALHOST":         "",
		"unknown":           "",
		"(none)":            "",
	}
	for in, want := range cases {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestWrapGathererWithRegistry 端到端验证：真实 Registry 输出的指标都被加上 instance。
func TestWrapGathererWithRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "test_metric",
		Help:        "测试用",
		ConstLabels: prometheus.Labels{"kind": "unit-test"},
	})
	gauge.Set(42)
	reg.MustRegister(gauge)

	families, err := WrapGatherer(reg, "prod-web-01").Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("没有采集到指标族")
	}

	found := false
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "instance" && lp.GetValue() == "prod-web-01" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("真实 Registry 输出的指标没有被加上 instance 标签")
	}
}
