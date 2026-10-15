package smart

import "github.com/prometheus/client_golang/prometheus"

// descs 集中保存 S.M.A.R.T. 相关的全部 metric 描述符。
type descs struct {
	up          *prometheus.Desc
	health      *prometheus.Desc
	standby     *prometheus.Desc
	temperature *prometheus.Desc
	rotational  *prometheus.Desc
	attribute   *prometheus.Desc

	powerOnHours     *prometheus.Desc
	powerOnHoursAttr *prometheus.Desc
	powerCycles      *prometheus.Desc

	reallocatedSectors    *prometheus.Desc
	reallocatedEvents     *prometheus.Desc
	currentPendingSectors *prometheus.Desc
	offlineUncorrectable  *prometheus.Desc
	crcErrors             *prometheus.Desc

	mediaWearout     *prometheus.Desc
	percentageUsed   *prometheus.Desc
	availableSpare   *prometheus.Desc
	dataUnitsWritten *prometheus.Desc
	mediaErrors      *prometheus.Desc
	unsafeShutdowns  *prometheus.Desc

	devicesExported *prometheus.Desc
	scrapeDuration  *prometheus.Desc
	scrapeError     *prometheus.Desc
}

// D 保存进程内共享的描述符集合。
var D = newDescs()

// newDescs 构造全部 metric 描述符。
//
// 绝大多数指标都带 device/model/serial 三个标签：
// 用 serial 聚合跨重启的曲线更可靠，用 device 关联具体盘位，
// model 则用于人类识别。
func newDescs() descs {
	disk := []string{"device", "model", "serial"}

	return descs{
		up: prometheus.NewDesc(
			namespace+"_disk_smart_up",
			"该设备是否存在并已导出 SMART 数据（1=是, 0=否）",
			append(append([]string{}, disk...), "interface"), nil),
		health: prometheus.NewDesc(
			namespace+"_disk_smart_health",
			"S.M.A.R.T. 整体健康判定（1=健康, 0=异常或未知）", disk, nil),
		standby: prometheus.NewDesc(
			namespace+"_disk_smart_standby",
			"硬盘是否处于待机/休眠状态（1=待机，未读取 SMART 数据, 0=正常采集）", disk, nil),
		temperature: prometheus.NewDesc(
			namespace+"_disk_smart_temperature_celsius",
			"硬盘温度（℃）", disk, nil),
		rotational: prometheus.NewDesc(
			namespace+"_disk_smart_rotational",
			"是否为机械硬盘（1=HDD, 0=SSD；无法判断时不输出）", disk, nil),
		attribute: prometheus.NewDesc(
			namespace+"_disk_smart_attribute_raw_value",
			"S.M.A.R.T. 属性原始值（RAW_VALUE），可用于自定义面板",
			append(append([]string{}, disk...), "id", "name"), nil),

		powerOnHours: prometheus.NewDesc(
			namespace+"_disk_smart_power_on_hours_total",
			"硬盘累计通电时间（小时）", disk, nil),
		powerOnHoursAttr: prometheus.NewDesc(
			namespace+"_disk_smart_power_on_hours_attr_total",
			"硬盘累计通电时间（小时，来自 ATA 属性 9）", disk, nil),
		powerCycles: prometheus.NewDesc(
			namespace+"_disk_smart_power_cycles_total",
			"硬盘累计上电次数（ATA 属性 12）", disk, nil),

		reallocatedSectors: prometheus.NewDesc(
			namespace+"_disk_smart_reallocated_sectors_total",
			"重新分配扇区数（ATA 属性 5），持续增长意味着盘体开始磨损", disk, nil),
		reallocatedEvents: prometheus.NewDesc(
			namespace+"_disk_smart_reallocated_events_total",
			"重映射事件次数（ATA 属性 196）", disk, nil),
		currentPendingSectors: prometheus.NewDesc(
			namespace+"_disk_smart_current_pending_sectors_total",
			"待映射扇区数（ATA 属性 197），大于 0 通常意味着即将坏道", disk, nil),
		offlineUncorrectable: prometheus.NewDesc(
			namespace+"_disk_smart_offline_uncorrectable_total",
			"离线不可修正扇区数（ATA 属性 198）", disk, nil),
		crcErrors: prometheus.NewDesc(
			namespace+"_disk_smart_crc_errors_total",
			"CRC 校验错误数（ATA 属性 199），突然增长通常说明数据线有问题", disk, nil),

		mediaWearout: prometheus.NewDesc(
			namespace+"_disk_smart_media_wearout",
			"介质寿命指标（Media_Wearout_Indicator / SSD_Life_Left，数值越大越新）", disk, nil),
		percentageUsed: prometheus.NewDesc(
			namespace+"_disk_smart_percentage_used",
			"SSD 寿命已用百分比（Percentage_Used）", disk, nil),
		availableSpare: prometheus.NewDesc(
			namespace+"_disk_smart_available_spare",
			"NVMe 备用空间百分比（Available_Spare）", disk, nil),
		dataUnitsWritten: prometheus.NewDesc(
			namespace+"_disk_smart_data_units_written_total",
			"NVMe 已写入数据量（单位 1000 x 512 字节）", disk, nil),
		mediaErrors: prometheus.NewDesc(
			namespace+"_disk_smart_media_errors_total",
			"NVMe 介质与数据完整性错误数", disk, nil),
		unsafeShutdowns: prometheus.NewDesc(
			namespace+"_disk_smart_unsafe_shutdowns_total",
			"NVMe 非正常断电次数", disk, nil),

		devicesExported: prometheus.NewDesc(
			namespace+"_disk_smart_devices",
			"本次导出的硬盘数量", nil, nil),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_disk_smart_scrape_duration_seconds", "S.M.A.R.T. 采集耗时（秒）", nil, nil),
		scrapeError: prometheus.NewDesc(
			namespace+"_disk_smart_scrape_error", "S.M.A.R.T. 采集是否出错（1=出错, 0=正常）", nil, nil),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range D.all() {
		ch <- d
	}
}

// all 返回全部描述符，供 Describe 使用。
func (d descs) all() []*prometheus.Desc {
	return []*prometheus.Desc{
		d.up, d.health, d.standby, d.temperature, d.rotational, d.attribute,
		d.powerOnHours, d.powerOnHoursAttr, d.powerCycles,
		d.reallocatedSectors, d.reallocatedEvents, d.currentPendingSectors,
		d.offlineUncorrectable, d.crcErrors,
		d.mediaWearout, d.percentageUsed, d.availableSpare,
		d.dataUnitsWritten, d.mediaErrors, d.unsafeShutdowns,
		d.devicesExported, d.scrapeDuration, d.scrapeError,
	}
}
