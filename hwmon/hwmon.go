// Package hwmon 采集主机温度传感器指标，数据全部来自 sysfs：
//
//	/sys/class/hwmon/hwmon*        主板/SoC/硬盘控制器的温度探头
//	/sys/class/thermal/thermal_zone*  通用 thermal 框架的温度区
//
// 不依赖 lm_sensors 等外部命令。
package hwmon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"node_exporter/internal/util"
)

const namespace = "node"

// tempInputSuffix 是 hwmon 温度读数文件的后缀。
const tempInputSuffix = "_input"

// Sensor 表示一个温度探头的读数（摄氏度）。
type Sensor struct {
	Chip  string  // 芯片名，如 coretemp / nvme / acpitz
	Label string  // 探头名，如 Core 0 / Package id 0 / Composite
	Value float64 // 当前温度（℃）
	Crit  *float64
	Max   *float64
}

// ThermalZone 表示一个 thermal zone 的读数。
type ThermalZone struct {
	Zone string  // 目录名，如 thermal_zone0
	Type string  // 目录下的 type 文件内容，如 x86_pkg_temp
	Temp float64 // 摄氏度
}

// hwmonGlob 是 hwmon 设备目录的前缀。
const hwmonGlob = "hwmon"

// ParseMilliCelsius 把 sysfs 中的毫摄氏度整数转换为摄氏度。
// 例如 "42000" -> 42.0。无法解析时返回 0。
func ParseMilliCelsius(s string) float64 {
	return util.ParseFloat64(strings.TrimSpace(s)) / 1000.0
}

// ReadHwmonSensors 遍历 /sys/class/hwmon 下的所有 hwmon 设备并读取温度。
//
// 每个设备目录下形如 temp1_input 的文件是读数（毫摄氏度），
// temp1_label 是探头名称，temp1_crit / temp1_max 是临界温度。
// sysRoot 通常是 "/sys"。
func ReadHwmonSensors(sysRoot string) ([]Sensor, error) {
	base := filepath.Join(sysRoot, "class", "hwmon")

	dirs, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", base, err)
	}

	var sensors []Sensor
	// /sys/class/hwmon/hwmonN 本身是指向 /sys/devices/... 的符号链接，
	// DirEntry.IsDir() 对链接返回 false，因此这里只按名字前缀筛选，
	// 真正的目录/文件判断交给后续的 os.ReadDir 与 os.ReadFile。
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), hwmonGlob) {
			continue
		}
		names = append(names, d.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		dir := filepath.Join(base, name)

		// 芯片名：优先用 name 文件（如 "coretemp"），否则退回目录名。
		chip := name
		if s, err := util.ReadString(filepath.Join(dir, "name")); err == nil {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				chip = trimmed
			}
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		fileNames := make([]string, 0, len(entries))
		for _, e := range entries {
			fileNames = append(fileNames, e.Name())
		}
		sort.Strings(fileNames)

		for _, file := range fileNames {
			if !strings.HasPrefix(file, "temp") || !strings.HasSuffix(file, tempInputSuffix) {
				continue
			}
			raw, err := util.ReadString(filepath.Join(dir, file))
			if err != nil {
				continue
			}
			chanName := strings.TrimSuffix(file, tempInputSuffix) // 例如 temp1
			s := Sensor{
				Chip:  chip,
				Label: hwmonSensorLabel(dir, chanName, chip),
				Value: ParseMilliCelsius(raw),
			}
			s.Crit = readOptionalTemp(filepath.Join(dir, chanName+"_crit"))
			s.Max = readOptionalTemp(filepath.Join(dir, chanName+"_max"))
			sensors = append(sensors, s)
		}
	}
	return sensors, nil
}

// hwmonSensorLabel 解析探头名称：temp1_label 优先，其次用通道号。
func hwmonSensorLabel(dir, chanName, chip string) string {
	if s, err := util.ReadString(filepath.Join(dir, chanName+"_label")); err == nil {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			return trimmed
		}
	}
	// 没有 label 时用 tempN 中的 N 作为编号，保证标签稳定可预期。
	return strings.TrimPrefix(chanName, "temp")
}

// readOptionalTemp 读取可选的临界/最高温度文件。
// 文件不存在或无法解析时返回 nil（指标将不输出，而不是输出 0）。
func readOptionalTemp(path string) *float64 {
	raw, err := util.ReadString(path)
	if err != nil {
		return nil
	}
	v := ParseMilliCelsius(raw)
	if v == 0 {
		return nil
	}
	return &v
}

// ReadThermalZones 遍历 /sys/class/thermal 下的温度区。
func ReadThermalZones(sysRoot string) ([]ThermalZone, error) {
	base := filepath.Join(sysRoot, "class", "thermal")

	dirs, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", base, err)
	}

	var zones []ThermalZone
	// 与 hwmon 同理：thermal_zoneN 是符号链接，不能用 IsDir() 判断。
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), "thermal_zone") {
			continue
		}
		names = append(names, d.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		dir := filepath.Join(base, name)

		raw, err := util.ReadString(filepath.Join(dir, "temp"))
		if err != nil {
			continue
		}
		zoneType := name
		if s, err := util.ReadString(filepath.Join(dir, "type")); err == nil {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				zoneType = trimmed
			}
		}
		zones = append(zones, ThermalZone{
			Zone: name,
			Type: zoneType,
			Temp: ParseMilliCelsius(raw),
		})
	}
	return zones, nil
}

// Exporter 是温度指标的 Prometheus Collector。
type Exporter struct {
	sysRoot string

	tempCelsius *prometheus.Desc
	tempCrit    *prometheus.Desc
	tempMax     *prometheus.Desc
	zoneCelsius *prometheus.Desc

	sensorCount    *prometheus.Desc
	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 创建温度 Collector。sysRoot 通常是 "/sys"。
func New(sysRoot string) *Exporter {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	return &Exporter{
		sysRoot: sysRoot,

		tempCelsius: prometheus.NewDesc(
			namespace+"_hwmon_temp_celsius",
			"温度传感器读数（℃）",
			[]string{"chip", "sensor"}, nil,
		),
		tempCrit: prometheus.NewDesc(
			namespace+"_hwmon_temp_crit_celsius",
			"温度传感器临界值（℃），来自 sysfs 的 *_crit",
			[]string{"chip", "sensor"}, nil,
		),
		tempMax: prometheus.NewDesc(
			namespace+"_hwmon_temp_max_celsius",
			"温度传感器上限（℃），来自 sysfs 的 *_max",
			[]string{"chip", "sensor"}, nil,
		),
		zoneCelsius: prometheus.NewDesc(
			namespace+"_thermal_zone_temp_celsius",
			"thermal zone 温度（℃）",
			[]string{"zone", "type"}, nil,
		),

		sensorCount: prometheus.NewDesc(
			namespace+"_temperature_sensors",
			"本次采集到的温度探头数量", nil, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_temperature_scrape_duration_seconds", "温度指标采集耗时（秒）", nil, nil,
		),
		scrapeError: prometheus.NewDesc(
			namespace+"_temperature_scrape_error", "温度指标采集是否出错（1=出错, 0=正常）", nil, nil,
		),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.tempCelsius
	ch <- e.tempCrit
	ch <- e.tempMax
	ch <- e.zoneCelsius
	ch <- e.sensorCount
	ch <- e.scrapeDuration
	ch <- e.scrapeError
}

// Collect 实现 prometheus.Collector。
//
// 机器上没有 hwmon 设备时（例如容器、部分 ARM 设备）属于正常情况，
// 只会上报 sensor 数量为 0，而不会把 scrape_error 置为 1。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	var errs []string

	sensors, err := ReadHwmonSensors(e.sysRoot)
	if err != nil {
		// 没有 /sys/class/hwmon 不算错误（容器常见）。
		if !isNotExist(err) {
			errs = append(errs, "hwmon: "+err.Error())
		}
	}
	for _, s := range sensors {
		ch <- prometheus.MustNewConstMetric(e.tempCelsius, prometheus.GaugeValue, s.Value, s.Chip, s.Label)
		if s.Crit != nil {
			ch <- prometheus.MustNewConstMetric(e.tempCrit, prometheus.GaugeValue, *s.Crit, s.Chip, s.Label)
		}
		if s.Max != nil {
			ch <- prometheus.MustNewConstMetric(e.tempMax, prometheus.GaugeValue, *s.Max, s.Chip, s.Label)
		}
	}

	zones, err := ReadThermalZones(e.sysRoot)
	if err != nil && !isNotExist(err) {
		errs = append(errs, "thermal: "+err.Error())
	}
	for _, z := range zones {
		ch <- prometheus.MustNewConstMetric(e.zoneCelsius, prometheus.GaugeValue, z.Temp, z.Zone, z.Type)
	}

	ch <- prometheus.MustNewConstMetric(e.sensorCount, prometheus.GaugeValue, float64(len(sensors)+len(zones)))

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

// isNotExist 判断错误是否由路径不存在引起。
// ReadHwmonSensors/ReadThermalZones 都用 %w 包装了底层错误，
// 因此 errors.Is 可以穿透 fmt.Errorf。
func isNotExist(err error) bool {
	return err != nil && errors.Is(err, os.ErrNotExist)
}
