package hwmon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// writeSys 在伪 sysfs 中创建文件，自动建立父目录。
func writeSys(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", rel, err)
	}
}

// makeSysRoot 构造一个模拟的 /sys/class 目录树。
//
// 结构参考真实机器（CPU 温度 + NVMe 温度 + thermal zone）。
func makeSysRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// hwmon0: coretemp，两个核心 + package
	writeSys(t, root, "class/hwmon/hwmon0/name", "coretemp")
	writeSys(t, root, "class/hwmon/hwmon0/temp1_input", "45000")
	writeSys(t, root, "class/hwmon/hwmon0/temp1_label", "Core 0")
	writeSys(t, root, "class/hwmon/hwmon0/temp1_crit", "100000")
	writeSys(t, root, "class/hwmon/hwmon0/temp1_max", "90000")
	writeSys(t, root, "class/hwmon/hwmon0/temp2_input", "46500")
	writeSys(t, root, "class/hwmon/hwmon0/temp2_label", "Core 1")

	// hwmon1: nvme，带 label 与 crit/max
	writeSys(t, root, "class/hwmon/hwmon1/name", "nvme")
	writeSys(t, root, "class/hwmon/hwmon1/temp1_input", "31850")
	writeSys(t, root, "class/hwmon/hwmon1/temp1_label", "Composite")
	writeSys(t, root, "class/hwmon/hwmon1/temp1_crit", "84850")
	writeSys(t, root, "class/hwmon/hwmon1/temp1_max", "82850")

	// hwmon2: 只有通道号、没有 name 与 label 文件
	writeSys(t, root, "class/hwmon/hwmon2/temp1_input", "40000")

	// 与温度无关的文件，应被忽略
	writeSys(t, root, "class/hwmon/hwmon2/in0_input", "33000")
	writeSys(t, root, "class/hwmon/hwmon2/fan1_input", "1200")

	// thermal zone
	writeSys(t, root, "class/thermal/thermal_zone0/type", "x86_pkg_temp")
	writeSys(t, root, "class/thermal/thermal_zone0/temp", "46000")

	return root
}

func TestParseMilliCelsius(t *testing.T) {
	cases := map[string]float64{
		"42000":    42,
		"0":        0,
		"31850":    31.85,
		" 46000\n": 46,
	}
	for in, want := range cases {
		if got := ParseMilliCelsius(in); got != want {
			t.Errorf("ParseMilliCelsius(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestReadHwmonSensors(t *testing.T) {
	root := makeSysRoot(t)

	sensors, err := ReadHwmonSensors(root)
	if err != nil {
		t.Fatalf("ReadHwmonSensors 失败: %v", err)
	}
	// 期望 4 个探头：coretemp 两个 + nvme 一个 + 无名芯片一个。
	if len(sensors) != 4 {
		t.Fatalf("解析到 %d 个探头，期望 4: %+v", len(sensors), sensors)
	}

	// 按 chip/sensor 索引。
	index := map[string]Sensor{}
	for _, s := range sensors {
		index[s.Chip+"/"+s.Label] = s
	}

	if s, ok := index["coretemp/Core 0"]; !ok {
		t.Error("缺少 coretemp/Core 0 探头")
	} else {
		if s.Value != 45 {
			t.Errorf("coretemp/Core 0 = %v℃，期望 45", s.Value)
		}
		if s.Crit == nil || *s.Crit != 100 {
			t.Errorf("coretemp/Core 0 crit = %v，期望 100", s.Crit)
		}
		if s.Max == nil || *s.Max != 90 {
			t.Errorf("coretemp/Core 0 max = %v，期望 90", s.Max)
		}
	}

	if s, ok := index["nvme/Composite"]; !ok {
		t.Error("缺少 nvme/Composite 探头")
	} else {
		if s.Value != 31.85 {
			t.Errorf("nvme/Composite = %v℃，期望 31.85", s.Value)
		}
		if s.Crit == nil || *s.Crit != 84.85 {
			t.Errorf("nvme crit = %v，期望 84.85", s.Crit)
		}
	}

	// 没有 name 文件时退回目录名 hwmon2；没有 label 时用通道号。
	if s, ok := index["hwmon2/1"]; !ok {
		t.Errorf("缺少 hwmon2/1 探头，实际有: %+v", sensors)
	} else if s.Value != 40 {
		t.Errorf("hwmon2/1 = %v℃，期望 40", s.Value)
	}

	// Core 1 没有 crit/max 文件，对应指标应缺失（nil）而不是 0。
	if s := index["coretemp/Core 1"]; s.Crit != nil || s.Max != nil {
		t.Errorf("coretemp/Core 1 不应有 crit/max，实际 %v / %v", s.Crit, s.Max)
	}
}

func TestReadThermalZones(t *testing.T) {
	root := makeSysRoot(t)

	zones, err := ReadThermalZones(root)
	if err != nil {
		t.Fatalf("ReadThermalZones 失败: %v", err)
	}
	if len(zones) != 1 {
		t.Fatalf("解析到 %d 个 thermal zone，期望 1", len(zones))
	}
	if zones[0].Type != "x86_pkg_temp" {
		t.Errorf("type = %q，期望 x86_pkg_temp", zones[0].Type)
	}
	if zones[0].Temp != 46 {
		t.Errorf("temp = %v℃，期望 46", zones[0].Temp)
	}
}

// TestCollect 验证指标输出与 sysfs 内容对应。
func TestCollect(t *testing.T) {
	root := makeSysRoot(t)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(root))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	byName := map[string]*dto.MetricFamily{}
	for _, mf := range families {
		byName[mf.GetName()] = mf
	}

	temp := byName["node_hwmon_temp_celsius"]
	if temp == nil {
		t.Fatal("缺少 node_hwmon_temp_celsius")
	}
	if len(temp.GetMetric()) != 4 {
		t.Errorf("node_hwmon_temp_celsius 有 %d 个样本，期望 4", len(temp.GetMetric()))
	}

	crit := byName["node_hwmon_temp_crit_celsius"]
	if crit == nil || len(crit.GetMetric()) != 2 {
		t.Errorf("node_hwmon_temp_crit_celsius 样本数不对（只有带 crit 的探头才输出）")
	}

	zone := byName["node_thermal_zone_temp_celsius"]
	if zone == nil || len(zone.GetMetric()) != 1 {
		t.Fatal("缺少 node_thermal_zone_temp_celsius")
	}

	// 5 个温度来源（4 个 hwmon + 1 个 zone）。
	if s, ok := byName["node_temperature_sensors"]; !ok {
		t.Error("缺少 node_temperature_sensors")
	} else if v := s.GetMetric()[0].GetGauge().GetValue(); v != 5 {
		t.Errorf("node_temperature_sensors = %v，期望 5", v)
	}

	// 一切正常时不应报错。
	if s, ok := byName["node_temperature_scrape_error"]; !ok {
		t.Error("缺少 node_temperature_scrape_error")
	} else if v := s.GetMetric()[0].GetGauge().GetValue(); v != 0 {
		t.Errorf("node_temperature_scrape_error = %v，期望 0", v)
	}
}

// TestCollectWithoutHwmon 验证没有 hwmon 的环境（如容器）不会报错。
func TestCollectWithoutHwmon(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(New(t.TempDir()))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}

	for _, mf := range families {
		if mf.GetName() == "node_temperature_scrape_error" {
			if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 0 {
				t.Errorf("没有 hwmon 时 scrape_error = %v，期望 0", v)
			}
			return
		}
	}
	t.Fatal("缺少 node_temperature_scrape_error")
}
