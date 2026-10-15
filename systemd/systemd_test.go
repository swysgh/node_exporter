package systemd

import (
	"testing"

	"node_exporter/internal/util"
)

// listUnitsSample 摘自真实 `systemctl list-units --all --no-legend --plain --full --type=service`。
const listUnitsSample = `accounts-daemon.service                loaded    active   running Accounts Service
alsa-restore.service                   loaded    active   exited  Save/Restore Sound Card State
alsa-state.service                     loaded    inactive dead    Manage Sound Card State (restore and store)
anacron.service                        loaded    inactive dead    Run anacron jobs
cron.service                          loaded    active   running Regular background program processing daemon
docker.service                         loaded    active   running Docker Application Container Engine
failed-unit.service                    loaded    failed   failed  Something went wrong with this unit
masked.service                        masked    inactive dead    A masked unit
`

func TestParseListUnits(t *testing.T) {
	units := ParseListUnits(listUnitsSample)
	if len(units) != 8 {
		t.Fatalf("解析到 %d 个 unit，期望 8: %+v", len(units), units)
	}

	// 结果应按名称排序。
	if units[0].Name != "accounts-daemon.service" {
		t.Errorf("第一个 unit = %q，期望按名称排序", units[0].Name)
	}

	// 找一个 description 含空格的 unit，验证没有把描述切碎。
	var found bool
	for _, u := range units {
		if u.Name == "docker.service" {
			found = true
			if u.Load != "loaded" || u.Active != "active" || u.Sub != "running" {
				t.Errorf("docker.service = %+v", u)
			}
			if u.Description != "Docker Application Container Engine" {
				t.Errorf("description = %q", u.Description)
			}
		}
	}
	if !found {
		t.Error("未找到 docker.service")
	}

	for _, u := range units {
		if u.Name == "failed-unit.service" {
			if u.Active != "failed" || u.Sub != "failed" {
				t.Errorf("failed unit 解析错误: %+v", u)
			}
		}
		if u.Name == "masked.service" {
			if u.Load != "masked" {
				t.Errorf("masked unit 的 load = %q，期望 masked", u.Load)
			}
		}
	}
}

// TestParseListUnitsSkipsJunk 验证表头与告警行被跳过。
func TestParseListUnitsSkipsJunk(t *testing.T) {
	content := `ACTIVE SUB DESCRIPTION
● foo.service - some description
  badline

cron.service loaded active running Cron daemon
`
	units := ParseListUnits(content)
	if len(units) != 1 {
		t.Fatalf("解析到 %d 个 unit，期望 1: %+v", len(units), units)
	}
	if units[0].Name != "cron.service" {
		t.Errorf("Name = %q", units[0].Name)
	}
}

// showSample 摘自真实 `systemctl show --property=... cron.service dbus.service`。
//
// 关键点：Id 既不是第一个也不是最后一个属性，因此解析必须按块处理，
// 否则后一个 unit 的内存值会被算到前一个 unit 上。
const showSample = `NRestarts=0
MemoryCurrent=10162176
CPUUsageNSec=43706975000
TasksCurrent=1
Id=cron.service
LoadState=loaded
ActiveState=active
SubState=running

NRestarts=2
MemoryCurrent=6262784
CPUUsageNSec=296100432000
TasksCurrent=7
Id=dbus.service
LoadState=loaded
ActiveState=active
SubState=running

`

func TestParseShowOutput(t *testing.T) {
	details := ParseShowOutput(showSample)
	if len(details) != 2 {
		t.Fatalf("解析到 %d 个 unit，期望 2: %+v", len(details), details)
	}

	cron, ok := details["cron.service"]
	if !ok {
		t.Fatal("缺少 cron.service")
	}
	if cron.MemoryCurrent != 10162176 {
		t.Errorf("cron MemoryCurrent = %v，期望 10162176", cron.MemoryCurrent)
	}
	if cron.TasksCurrent != 1 {
		t.Errorf("cron TasksCurrent = %v，期望 1", cron.TasksCurrent)
	}
	if cron.NRestarts != 0 {
		t.Errorf("cron NRestarts = %v，期望 0", cron.NRestarts)
	}
	if cron.ActiveState != "active" || cron.SubState != "running" {
		t.Errorf("cron 状态解析错误: %+v", cron)
	}
	// 43706975000 纳秒 -> 43.706975 秒
	if got := cron.CPUUsageNSec; got < 43.70 || got > 43.71 {
		t.Errorf("cron CPUUsageNSec = %v，期望约 43.707", got)
	}

	dbus, ok := details["dbus.service"]
	if !ok {
		t.Fatal("缺少 dbus.service")
	}
	// 关键断言：dbus 的数值不能被算到 cron 上。
	if dbus.MemoryCurrent != 6262784 {
		t.Errorf("dbus MemoryCurrent = %v，期望 6262784", dbus.MemoryCurrent)
	}
	if dbus.TasksCurrent != 7 {
		t.Errorf("dbus TasksCurrent = %v，期望 7", dbus.TasksCurrent)
	}
	if dbus.NRestarts != 2 {
		t.Errorf("dbus NRestarts = %v，期望 2", dbus.NRestarts)
	}
	if got := dbus.CPUUsageNSec; got < 296.09 || got > 296.11 {
		t.Errorf("dbus CPUUsageNSec = %v，期望约 296.10", got)
	}
}

// TestParseShowOutputWithoutBlankLines 验证没有空行分隔时也能按 Id 切块。
func TestParseShowOutputWithoutBlankLines(t *testing.T) {
	content := `Id=a.service
MemoryCurrent=100
Id=b.service
MemoryCurrent=200
`
	details := ParseShowOutput(content)
	if len(details) != 2 {
		t.Fatalf("解析到 %d 个 unit，期望 2: %+v", len(details), details)
	}
	if details["a.service"].MemoryCurrent != 100 || details["b.service"].MemoryCurrent != 200 {
		t.Errorf("切块错误: %+v", details)
	}
}

// TestParseShowOutputSpecialValues 验证 [not set] 等特殊值被当作 0。
func TestParseShowOutputSpecialValues(t *testing.T) {
	content := `Id=stopped.service
MemoryCurrent=[not set]
CPUUsageNSec=[not set]
TasksCurrent=[not set]
NRestarts=0
`
	d := ParseShowOutput(content)["stopped.service"]
	if d.MemoryCurrent != 0 || d.CPUUsageNSec != 0 || d.TasksCurrent != 0 {
		t.Errorf("特殊值应解析为 0: %+v", d)
	}
}

func TestParseDurationSeconds(t *testing.T) {
	cases := map[string]float64{
		"43706975000":  43.706975, // 纯纳秒
		"1s 234567890": 1.23456789,
		"500ms":        0.5,
		"2min":         120,
		"1h":           3600,
		"90":           90 / 1e9, // 裸数字按纳秒处理
		"[not set]":    0,
		"":             0,
		"1.5s":         1.5,
	}
	for in, want := range cases {
		got := parseDurationSeconds(in)
		if diff := got - want; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("parseDurationSeconds(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]float64{
		"12582912":   12582912,
		"[not set]":  0,
		"[infinity]": 0,
		"0":          0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %v，期望 %v", in, got, want)
		}
	}
}

// TestNewDefaults 验证默认值填充。
func TestNewDefaults(t *testing.T) {
	e := New(Options{})
	if e.bin != "systemctl" {
		t.Errorf("bin = %q，期望 systemctl", e.bin)
	}
	if len(e.types) != 1 || e.types[0] != DefaultTypes {
		t.Errorf("types = %v，期望 [service]", e.types)
	}
	if e.timeout <= 0 {
		t.Errorf("timeout = %v，期望正值", e.timeout)
	}

	e = New(Options{Types: []string{" service ", "socket", ""}})
	if len(e.types) != 2 || e.types[0] != "service" || e.types[1] != "socket" {
		t.Errorf("types = %v，期望 [service socket]（应去掉空白与空项）", e.types)
	}
}

// TestFilterAppliedToUnits 验证筛选规则只影响导出的 unit。
func TestFilterAppliedToUnits(t *testing.T) {
	filter, err := util.NewFilter("^(cron|docker)", "")
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}

	units := ParseListUnits(listUnitsSample)
	var matched []string
	for _, u := range units {
		if filter.Match(u.Name) {
			matched = append(matched, u.Name)
		}
	}
	if len(matched) != 2 {
		t.Fatalf("筛选出 %d 个 unit，期望 2: %v", len(matched), matched)
	}
	if matched[0] != "cron.service" || matched[1] != "docker.service" {
		t.Errorf("筛选结果 = %v", matched)
	}
}

func TestStatesOneHot(t *testing.T) {
	// one-hot 标签集合应覆盖常见的 ActiveState 取值。
	want := map[string]bool{
		"active": true, "activating": true, "deactivating": true,
		"reloading": true, "failed": true, "inactive": true,
	}
	if len(states) != len(want) {
		t.Fatalf("states 数量 = %d，期望 %d", len(states), len(want))
	}
	for _, s := range states {
		if !want[s] {
			t.Errorf("unexpected state %q", s)
		}
	}
}
