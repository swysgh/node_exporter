package smart

import (
	"regexp"
	"testing"
)

// matchRegex 是测试辅助函数。
func matchRegex(expr, s string) bool {
	re, err := regexp.Compile(expr)
	if err != nil {
		panic(err)
	}
	return re.MatchString(s)
}

// sataSample 摘自真实 SATA 硬盘的 `smartctl -a` 输出。
const sataSample = `smartctl 7.3 2022-02-28 r5338 [x86_64-linux] (local build)

=== START OF INFORMATION SECTION ===
Model Family:     Western Digital Red
Device Model:     WDC WD40EFRX-68N32N0
Serial Number:    WD-WCC7K4PL0J1V
LU WWN Device Id: 5 0014ee 20b2e1234
Firmware Version: 82.00A82
User Capacity:    4,000,787,030,016 bytes [4.00 TB]
Sector Size:      512 bytes logical/physical
Rotation Rate:    5400 rpm
Device is:        In smartctl database
ATA Version is:   ACS-3 T13/2161-D revision 5
SATA Version is:  SATA 3.1, 6.0 Gb/s (current: 6.0 Gb/s)
Local Time is:    Thu Oct  2 16:00:00 2026 UTC
SMART support is: Available - device has SMART capability.
SMART support is: Enabled

=== START OF READ SMART DATA SECTION ===
SMART overall-health self-assessment test result: PASSED

SMART Attributes Data Structure revision number: 16
Vendor Specific SMART Attributes with Thresholds:
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  1 Raw_Read_Error_Rate     0x002f   200   200   051    Pre-fail  Always       -       0
  3 Spin_Up_Time            0x0027   178   178   021    Pre-fail  Always       -       4178
  5 Reallocated_Sector_Ct   0x0033   200   200   140    Pre-fail  Always       -       0
  9 Power_On_Hours          0x0032   089   089   000    Old_age   Always       -       27631
 12 Power_Cycle_Count       0x0032   200   200   000    Old_age   Always       -       118
184 End-to-End_Error        0x0032   100   100   099    Old_age   Always       -       0
187 Reported_Uncorrect      0x0032   100   100   000    Old_age   Always       -       0
188 Command_Timeout         0x0032   100   100   000    Old_age   Always       -       0
190 Airflow_Temperature_Cel 0x0022   113   106   067    Old_age   Always       -       37
194 Temperature_Celsius     0x0022   113   106   000    Old_age   Always       -       37
197 Current_Pending_Sector  0x0012   200   200   000    Old_age   Always       -       0
198 Offline_Uncorrectable   0x0010   100   253   000    Old_age   Offline      -       0
199 UDMA_CRC_Error_Count    0x003e   200   200   000    Old_age   Always       -       0
240 Head_Flying_Hours       0x0032   100   253   000    Old_age   Offline      -       27298
241 Total_LBAs_Written      0x0032   200   200   000    Old_age   Offline      -       10485760

SMART Error Log Version: 1
No Errors Logged

SMART Self-test log structure revision number 1
Num  Test_Description    Status                  Remaining  LifeTime(hours)  LBA_of_first_error
# 1  Short offline       Completed without error       00%     27631         -

Device is in STANDBY mode, exit(2)
`

func TestParseInfoSATA(t *testing.T) {
	info := ParseInfo("/dev/sda", sataSample)

	if info.Device != "/dev/sda" {
		t.Errorf("Device = %q", info.Device)
	}
	if info.Model != "WDC WD40EFRX-68N32N0" {
		t.Errorf("Model = %q", info.Model)
	}
	if info.Serial != "WD-WCC7K4PL0J1V" {
		t.Errorf("Serial = %q", info.Serial)
	}
	if info.Interface != "sat" {
		t.Errorf("Interface = %q，期望 sat", info.Interface)
	}
	if !info.HealthOK {
		t.Error("健康状态应为 PASSED/健康")
	}
	if info.Health != 1 {
		t.Errorf("Health = %v，期望 1", info.Health)
	}

	// 温度：属性 194 Temperature_Celsius = 37
	if info.Temperature == nil || *info.Temperature != 37 {
		t.Errorf("Temperature = %v，期望 37", info.Temperature)
	}

	// 机械硬盘。
	if info.Rotational == nil || *info.Rotational != 1 {
		t.Errorf("Rotational = %v，期望 1 (HDD)", info.Rotational)
	}

	// 通电时间：属性 9。
	if info.PowerOnHours == nil || *info.PowerOnHours != 27631 {
		t.Errorf("PowerOnHours = %v，期望 27631", info.PowerOnHours)
	}

	// 待机状态。
	if !info.Standby {
		t.Error("应当识别出 STANDBY 状态")
	}

	// 常用错误计数。
	cases := map[int]float64{
		5:   0,     // Reallocated_Sector_Ct
		9:   27631, // Power_On_Hours
		12:  118,   // Power_Cycle_Count
		197: 0,     // Current_Pending_Sector
		198: 0,     // Offline_Uncorrectable
		199: 0,     // UDMA_CRC_Error_Count
	}
	for id, want := range cases {
		got, ok := info.Attr(id)
		if !ok {
			t.Errorf("缺少属性 %d", id)
			continue
		}
		if got != want {
			t.Errorf("属性 %d = %v，期望 %v", id, got, want)
		}
	}

	// 非错误类属性不应被误认成错误计数。
	if v, ok := info.Attr(190); !ok || v != 37 {
		t.Errorf("属性 190 (Airflow_Temperature_Cel) = %v (ok=%v)，期望 37", v, ok)
	}

	// 属性总数应当覆盖整张表（这里有 15 行）。
	if len(info.Attributes) < 15 {
		t.Errorf("解析到 %d 个属性，期望至少 15 个", len(info.Attributes))
	}
}

// nvmeSample 摘自真实 NVMe SSD 的 `smartctl -a` 输出。
const nvmeSample = `smartctl 7.3 2022-02-28 r5338 [x86_64-linux] (local build)

=== START OF INFORMATION SECTION ===
Model Number:                       Samsung SSD 970 EVO Plus 1TB
Serial Number:                      S4EUNF0M123456
Firmware Version:                   2B2QEXM7
PCI Vendor/Subsystem ID:            0x144d
Total NVM Capacity:                 1,000,204,886,016 [1.00 TB]
Namespace 1 Size/Capacity:           1,000,204,886,016 [1.00 TB]
Namespace 1 Formatted LBA Size:     512
Local Time is:                      Thu Oct  2 16:00:00 2026 UTC
Firmware Updates (NVMe Log 0x06):    3 Slots, no reset available
NVMe Version:                       1.3

=== START OF SMART DATA SECTION ===
SMART overall-health self-assessment test result: PASSED

SMART/Health Information (NVMe Log 0x02)
Critical Warning:                   0x00
Temperature:                        41 Celsius
Available Spare:                    100%
Available Spare Threshold:          10%
Percentage Used:                    3%
Data Units Read:                    12,345,678 [6.32 TB]
Data Units Written:                 23,456,789 [12.0 TB]
Host Read Commands:                 987,654,321
Host Write Commands:                876,543,210
Controller Busy Time:               1,234
Power Cycles:                       342
Power On Hours:                     5,678
Unsafe Shutdowns:                   19
Media and Data Integrity Errors:    0
Error Information Log Entries:      0
Warning  Comp. Temperature Time:    0
Critical Comp. Temperature Time:    0
Temperature Sensor 1:               41 Celsius
`

func TestParseInfoNVMe(t *testing.T) {
	info := ParseInfo("/dev/nvme0n1", nvmeSample)

	if info.Model != "Samsung SSD 970 EVO Plus 1TB" {
		t.Errorf("Model = %q", info.Model)
	}
	if info.Serial != "S4EUNF0M123456" {
		t.Errorf("Serial = %q", info.Serial)
	}
	if info.Interface != "nvme" {
		t.Errorf("Interface = %q，期望 nvme", info.Interface)
	}
	if !info.HealthOK {
		t.Error("NVMe 健康状态应为正常")
	}

	if info.Temperature == nil || *info.Temperature != 41 {
		t.Errorf("Temperature = %v，期望 41", info.Temperature)
	}
	if info.PowerOnHours == nil || *info.PowerOnHours != 5678 {
		t.Errorf("PowerOnHours = %v，期望 5678", info.PowerOnHours)
	}
	if info.Standby {
		t.Error("不应识别为待机")
	}
	// NVMe 没有 ATA 属性表。
	if len(info.Attributes) != 0 {
		t.Errorf("NVMe 不应解析出 ATA 属性，实际 %+v", info.Attributes)
	}
}

func TestParseInfoNVMeHealthFields(t *testing.T) {
	// NVMe 的 Available Spare / Percentage Used / Data Units Written 等
	// 由 emit 阶段按属性名查找，这里确认解析器至少不会因这些行崩溃或误判健康。
	info := ParseInfo("/dev/nvme0n1", nvmeSample)
	if info.Health != 1 {
		t.Errorf("Health = %v，期望 1", info.Health)
	}
	if info.Rotational != nil {
		t.Errorf("NVMe 无 Rotation Rate 字段，Rotational 应为 nil，实际 %v", *info.Rotational)
	}
}

// failingSample 模拟硬盘故障时的输出。
const failingSample = `=== START OF INFORMATION SECTION ===
Device Model:     ST8000VN004
Serial Number:    ZDH1234
ATA Version is:   ACS-3
SATA Version is:  SATA 3.0, 6.0 Gb/s
Rotation Rate:    7200 rpm

SMART overall-health self-assessment test result: FAILED!

SMART Attributes Data Structure revision number: 10
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  5 Reallocated_Sector_Ct   0x0033   005   005   010    Pre-fail  Always   FAILING_NOW 4096
  9 Power_On_Hours          0x0032   040   040   000    Old_age   Always       -       49000
194 Temperature_Celsius     0x0022   120   105   000    Old_age   Always       -       45
`

func TestParseInfoFailed(t *testing.T) {
	info := ParseInfo("/dev/sdb", failingSample)

	if info.HealthOK {
		t.Error("FAILED! 不应判定为健康")
	}
	if info.Health != 0 {
		t.Errorf("Health = %v，期望 0", info.Health)
	}

	v, ok := info.Attr(5)
	if !ok || v != 4096 {
		t.Errorf("重分配扇区 = %v (ok=%v)，期望 4096", v, ok)
	}
	// WHEN_FAILED 列的 FAILING_NOW 不应破坏 RAW_VALUE 的解析。
	if info.Temperature == nil || *info.Temperature != 45 {
		t.Errorf("Temperature = %v，期望 45", info.Temperature)
	}
}

// ssataSample 模拟 SAS 硬盘（SCSI 走 SMART Health Status）。
const ssataSample = `=== START OF INFORMATION SECTION ===
Vendor:               SEAGATE
Product:              ST4000NM0023
Revision:             0004
Compliance:           SPC-4
User Capacity:        4,000,787,030,016 bytes [4.00 TB]
Rotation Rate:        7200 rpm
Transport protocol:   SAS (SPL-3)
Logical block size:   512 bytes
Current Drive Temperature:     36 C

=== START OF READ SMART DATA SECTION ===
SMART Health Status: OK

Current Drive Temperature:     36 C
`

func TestParseInfoSCSI(t *testing.T) {
	info := ParseInfo("/dev/sdd", ssataSample)

	if !info.HealthOK {
		t.Error("SCSI 健康状态应为 OK")
	}
	if info.Interface != "sat" {
		t.Errorf("Interface = %q，期望 sat", info.Interface)
	}
	// SCSI/SAS 输出只有 Vendor + Product 两行，应当拼成完整型号。
	if info.Model != "SEAGATE ST4000NM0023" {
		t.Errorf("Model = %q，期望 SEAGATE ST4000NM0023", info.Model)
	}
	if info.Temperature == nil || *info.Temperature != 36 {
		t.Errorf("Temperature = %v，期望 36", info.Temperature)
	}
}

func TestParseInfoUnhealthy(t *testing.T) {
	info := ParseInfo("/dev/sdc", "SMART Health Status: FAILURE PREDICTION THRESHOLD EXCEEDED\n")
	if info.HealthOK {
		t.Error("健康状态异常时应判定为不健康")
	}
	if info.Health != 0 {
		t.Errorf("Health = %v，期望 0", info.Health)
	}
}

func TestParseInfoEmpty(t *testing.T) {
	info := ParseInfo("/dev/sdz", "")
	if info.HealthOK {
		t.Error("空输出不应判定为健康（调用方应据此报警）")
	}
	if info.Health != 0 {
		t.Errorf("Health = %v，期望 0", info.Health)
	}
	if len(info.Attributes) != 0 {
		t.Errorf("空输出不应有属性: %+v", info.Attributes)
	}
}

func TestParseCelsiusValue(t *testing.T) {
	cases := map[string]float64{
		"Temperature:                        41 Celsius":   41,
		"Temperature:                        0x29 Celsius": 41,
		"Temperature_Celsius:                36":           36,
		"Current Drive Temperature:     36 C":              36,
	}
	for in, want := range cases {
		got, ok := parseCelsiusValue(in)
		if !ok || got != want {
			t.Errorf("parseCelsiusValue(%q) = (%v,%v)，期望 (%v,true)", in, got, ok, want)
		}
	}
	if _, ok := parseCelsiusValue("no colon here"); ok {
		t.Error("没有冒号时应当返回 ok=false")
	}
}

const scanSample = `/dev/sda -d sat # /dev/sda [SAT], ATA device
/dev/sdb -d sat # /dev/sdb [SAT], ATA device
/dev/nvme0 -d nvme # /dev/nvme0, NVMe device
/dev/sr0 -d scsi # /dev/sr0 [SCSI], Optical drive
/dev/zram0 -d sat # /dev/zram0
`

func TestParseScanDevices(t *testing.T) {
	devices := ParseScanDevices(scanSample)
	want := []string{"/dev/nvme0", "/dev/sda", "/dev/sdb", "/dev/sr0", "/dev/zram0"}

	if len(devices) != len(want) {
		t.Fatalf("解析到 %v，期望 %v", devices, want)
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Errorf("devices[%d] = %q，期望 %q（应按名称排序）", i, devices[i], want[i])
		}
	}
}

func TestParseScanDevicesEmpty(t *testing.T) {
	if got := ParseScanDevices(""); len(got) != 0 {
		t.Errorf("空输入 = %v", got)
	}
	if got := ParseScanDevices("garbage without device path\n"); len(got) != 0 {
		t.Errorf("非 /dev/ 开头的行应被忽略，实际 %v", got)
	}
}

// TestDefaultExcludeRegex 验证默认排除规则能滤掉无意义设备。
func TestDefaultExcludeRegex(t *testing.T) {
	excluded := []string{"/dev/zram0", "/dev/loop0", "/dev/sr0", "/dev/dm-0", "/dev/md0", "/dev/nbd0"}
	for _, dev := range excluded {
		if !matchRegex(DefaultExcludeRegex, dev) {
			t.Errorf("%q 应当被默认规则排除", dev)
		}
	}
	kept := []string{"/dev/sda", "/dev/sdb", "/dev/nvme0n1", "/dev/mapper/vg-lv"}
	for _, dev := range kept {
		if matchRegex(DefaultExcludeRegex, dev) {
			t.Errorf("%q 不应被默认规则排除", dev)
		}
	}
}

// TestNewDefaults 验证默认配置。
func TestNewDefaults(t *testing.T) {
	e := New(Options{})
	if e.opts.Bin != "smartctl" {
		t.Errorf("Bin = %q", e.opts.Bin)
	}
	if e.opts.Parallel != 4 {
		t.Errorf("Parallel = %d，期望 4", e.opts.Parallel)
	}
	if e.opts.Timeout <= 0 {
		t.Errorf("Timeout = %v", e.opts.Timeout)
	}

	// NoStandby 必须原样保留，不被构造函数改写。
	if e := New(Options{NoStandby: true}); !e.opts.NoStandby {
		t.Error("NoStandby=true 应当被保留")
	}
	if e := New(Options{NoStandby: false}); e.opts.NoStandby {
		t.Error("NoStandby=false 应当被保留")
	}
}
