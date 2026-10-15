# node_exporter

面向通用 Linux 主机的 Prometheus 指标采集器，用 Go 编写，单文件部署。

所有输出指标都带 `instance="<hostname>"` 标签（自动识别本机主机名，可用 `-hostname` 覆盖），
因此 Prometheus 里的实例名是主机名而不是 `IP:9100`。

## 功能

| 模块 | 采集内容 | 默认 | 开关 |
| --- | --- | --- | --- |
| CPU / 负载 | 各核 CPU 时间与使用率、1/5/15 分钟负载、上下文切换、进程数、开机时间、uname | 开启 | `-enable-cpu` |
| 内存 / swap | 内存明细、使用量与百分比、换页次数、大页统计 | 开启 | `-enable-mem` |
| 温度 | hwmon 温度探头、thermal zone（无需 lm_sensors） | 开启 | `-enable-temp` |
| 网卡流量 | 收发字节/包/错误/丢包、每秒速率、链路速率与双工，**支持按网卡筛选** | 开启 | `-enable-network` |
| systemd | unit 状态、内存、CPU 时间、任务数、重启次数，**支持按 unit 筛选** | 开启 | `-enable-systemd` |
| 文件系统 | 容量、可用/已用空间、使用率、inode、只读状态，**支持按挂载点/类型筛选** | 开启 | `-enable-filesystem` |
| 硬盘 SMART | 健康状态、温度、寿命、重分配/待映射/CRC 错误、通电时间 | **关闭** | `-enable-smart` |
| 运行时 | `go_*`、`process_*`（exporter 自身） | 开启 | `-enable-runtime` |

监听端口默认 **9100**，路径 `/metrics`。

## 快速开始

```bash
# 编译
make build

# 前台运行（默认模块）
./node_exporter

# 验证
curl -s localhost:9100/metrics | head
```

浏览器访问 `http://<host>:9100/` 也能看到当前 instance 与已启用模块。

### 配置文件

除命令行参数外，还可以用 `-config <path>` 读取一个 JSON 配置文件。
优先级为：**命令行显式给出的参数 > 配置文件 > 内置默认值**。
仓库根目录的 `config.example.json` 列出了全部可用字段。
配置文件使用 `DisallowUnknownFields` 解析，拼错的键会在启动时报错退出。

## 安装为 systemd 服务

```bash
sudo make install            # 安装二进制与 service 文件
sudo make install-service    # 安装并启动
```

`node_exporter.service` 里的 `ExecStart` 可以按需修改，例如开启 SMART：

```ini
ExecStart=/usr/local/bin/node_exporter \
  -web.listen-address=:9100 \
  -enable-smart -smart.device=/dev/sda,/dev/sdb,/dev/nvme0n1
```

> 采集 SMART 需要读取块设备（`/dev/sd*`、`/dev/nvme*`），
> 因此服务以 `root` 运行，或配置 udev 规则后改用普通用户 + `disk` 组。

## 常用参数

```bash
./node_exporter -h          # 查看全部参数
```

### 模块开关

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-web.listen-address` | `:9100` | 监听地址 |
| `-web.telemetry-path` | `/metrics` | 指标路径 |
| `-auth-token` | 空 | 非空时要求 `Authorization: Bearer <token>`，不匹配返回 401 |
| `-config` | 空 | JSON 配置文件路径 |
| `-hostname` | 空（自动识别） | 覆盖 instance 标签使用的 hostname |
| `-enable-system` | `true` | 基础系统指标总开关 |
| `-enable-cpu` / `-enable-mem` / `-enable-temp` | `true` | 需 `-enable-system` 为真才生效 |
| `-enable-systemd` / `-enable-network` | `true` | systemd、网卡流量 |
| `-enable-filesystem` | `true` | 文件系统容量（独立开关，与 `-enable-system` 无关） |
| `-enable-smart` | `false` | 硬盘 SMART |
| `-enable-runtime` | `true` | `go_*` 与 `process_*` |
| `-command-timeout` | `20s` | 调用外部命令的超时 |
| `-proc.root` / `-sys.root` | `/proc` / `/sys` | procfs/sysfs 根目录 |

### systemd 筛选

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-systemd.include` | 空 | 正则，只导出匹配的 unit，例如 `^(ssh\|cron\|docker)` |
| `-systemd.exclude` | 空 | 正则，排除匹配的 unit |
| `-systemd.types` | `service` | unit 类型，逗号分隔，如 `service,socket,timer` |
| `-systemd.detail` | `true` | 是否采集每个 unit 的内存/CPU/重启次数（会调用 `systemctl show`，较慢） |

筛选规则语义：`include` 命中才导出，然后再应用 `exclude`。两者都是 Go 正则，
非法正则会在启动时报错退出，不会静默失效。

```bash
# 只看 ssh / cron / docker 三个服务，且不要详情（更快）
./node_exporter -systemd.include='^(ssh|cron|docker)' -systemd.detail=false
```

### 网卡筛选

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-network.include` | 空 | 正则，只导出匹配的网卡 |
| `-network.exclude` | 内置虚拟网卡规则 | 正则，排除匹配的网卡；传空字符串可关闭默认排除 |

默认排除 `lo`、`veth*`、`docker*`、`br-*`、`virbr*`、`tun*`、`tap*`、`zt*`、`gre*`、`wg*` 等，
避免本机内部流量把物理网卡的曲线冲淡。

```bash
# 只看物理网卡
./node_exporter -network.include='^(eth|enp|eno|bond)'

# 连回环口一起看
./node_exporter -network.exclude=''

# 只看 eth0，排除 eth1
./node_exporter -network.include='^eth' -network.exclude='^eth1$'
```

筛选结果可通过 `node_network_interfaces`（导出数量）与 `node_network_interfaces_filtered`
（被排除数量）确认规则是否生效。

### 文件系统筛选

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-filesystem.include` | 空 | 正则，只导出匹配的挂载点 |
| `-filesystem.exclude` | 空 | 正则，排除匹配的挂载点 |
| `-filesystem.fstype-exclude` | 内置伪文件系统规则 | 正则，排除匹配的文件系统类型；传空字符串可采集全部类型 |
| `-filesystem.mounts-path` | `/proc/mounts` | 挂载表路径，便于测试注入；默认跟随 `-proc.root` |

默认排除 `tmpfs`、`devtmpfs`、`overlay`、`proc`、`sysfs`、`cgroup`、`fuse.lxcfs` 等伪文件系统。

```bash
# 只关心数据盘
./node_exporter -filesystem.include='^/(data|mnt)'

# 连 tmpfs 一起看
./node_exporter -filesystem.fstype-exclude=''
```

### SMART 参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-smart.path` | `smartctl` | 命令路径 |
| `-smart.device` | 空（自动发现） | 显式指定设备，逗号分隔 |
| `-smart.exclude` | 内置规则 | 排除 zram/loop/sr/dm/md/nbd 等无意义设备 |
| `-smart.parallel` | `4` | 并发执行 smartctl 的数量 |
| `-smart.standby` | `true` | 硬盘待机时不唤醒（等价 `-n standby`） |

## 指标一览

前缀统一为 `node_`，每个模块都有 `node_<模块>_scrape_error`（0 正常 / 1 出错）与
`node_<模块>_scrape_duration_seconds`，可直接用于监控采集本身是否健康。

### CPU / 负载 / 系统

```
node_cpu_seconds_total{cpu,mode}       CPU 累计时间（mode: user/nice/system/idle/iowait/irq/softirq/steal/guest/guest_nice；cpu="total" 为全核汇总）
node_cpu_usage_percent{cpu}            两次采样之间的 CPU 使用率
node_cpu_info{cpu,model,vendor,...}    CPU 静态信息
node_cpu_frequency_mhz{cpu}            当前主频
node_load1 / node_load_per_core_ratio{interval}
node_procs_running / node_procs_blocked
node_context_switches_total / node_forks_total
node_boot_time_seconds / node_uptime_seconds / node_entropy_available_bits
node_uname_info{sysname,nodename,release,version,machine,domainname}
```

### 内存 / swap

```
node_memory_total_bytes / node_memory_available_bytes / node_memory_used_bytes
node_memory_used_percent / node_memory_available_percent
node_memory_<字段>_bytes               Buffers/Cached/Slab/AnonPages/PageTables/CommitLimit/... 等
node_swap_total_bytes / node_swap_free_bytes / node_swap_used_bytes / node_swap_used_percent
node_swap_pages_total{direction="in"|"out"}
node_hugepages{kind} / node_hugepages_size_bytes
```

### 温度

```
node_hwmon_temp_celsius{chip,sensor}
node_hwmon_temp_crit_celsius{chip,sensor}
node_hwmon_temp_max_celsius{chip,sensor}
node_thermal_zone_temp_celsius{zone,type}
node_temperature_sensors
```

### 网卡流量

```
node_network_receive_bytes_total{device}      node_network_transmit_bytes_total{device}
node_network_receive_packets_total{device}    node_network_transmit_packets_total{device}
node_network_receive_errors_total{device}     node_network_transmit_errors_total{device}
node_network_receive_dropped_total{device,reason}     node_network_transmit_dropped_total{device,reason}
node_network_receive_bytes_per_second{device} node_network_transmit_bytes_per_second{device}
node_network_receive_packets_per_second{device} node_network_transmit_packets_per_second{device}
node_network_link_speed_bps{device} / node_network_link_duplex{device} / node_network_link_carrier{device}
node_network_info{device,operstate,duplex,mac_address}
node_network_interfaces / node_network_interfaces_filtered
```

> `*_bytes_per_second` 是 exporter 直接算出的速率（gauge）；
> 也可以用 `rate(node_network_receive_bytes_total[5m])` 自行计算。

### systemd

```
node_systemd_unit_state{unit,state}            one-hot：active/activating/deactivating/reloading/failed/inactive
node_systemd_unit_info{unit,load_state,active_state,sub_state,description}
node_systemd_unit_memory_bytes{unit}           需 -systemd.detail=true
node_systemd_unit_cpu_seconds_total{unit}      需 -systemd.detail=true
node_systemd_unit_tasks{unit}                  需 -systemd.detail=true
node_systemd_unit_restarts_total{unit}         需 -systemd.detail=true
node_systemd_units{type} / node_systemd_units_running{type} / node_systemd_units_failed{type}
```

### 文件系统

标签统一为 `device`、`mountpoint`、`fstype`。容量通过 `statfs(2)` 采集，不调用 `df`。
同一个设备可能对应多个挂载点（bind mount），因此按 `mountpoint` 出序列，不按设备去重。

```
node_filesystem_size_bytes            文件系统总容量
node_filesystem_free_bytes            空闲空间（含 root 保留块）
node_filesystem_avail_bytes           非特权用户可用空间
node_filesystem_used_bytes            = size - free
node_filesystem_used_percent          = (size - avail) / size * 100（与 df 口径一致）
node_filesystem_files / node_filesystem_files_free      inode 总数 / 空闲数
node_filesystem_readonly              1=只读 0=可读写
node_filesystem_mounts / node_filesystem_mounts_filtered  导出/被排除的挂载点数量
```

### SMART

```
node_disk_smart_health{device,model,serial}             1=健康
node_disk_smart_temperature_celsius{device,model,serial}
node_disk_smart_standby{device,...}                     1=硬盘待机，未读取数据
node_disk_smart_rotational{device,...}                  1=HDD 0=SSD
node_disk_smart_reallocated_sectors_total{device,...}
node_disk_smart_current_pending_sectors_total{device,...}
node_disk_smart_offline_uncorrectable_total{device,...}
node_disk_smart_crc_errors_total{device,...}
node_disk_smart_power_on_hours_total{device,...}
node_disk_smart_media_wearout / percentage_used / available_spare
node_disk_smart_attribute_raw_value{device,model,serial,id,name}
```

## Prometheus 配置示例

```yaml
scrape_configs:
  - job_name: node
    static_configs:
      - targets: ['192.168.1.10:9100', '192.168.1.11:9100']
    scrape_interval: 30s
```

因为 exporter 会主动上报 `instance="<hostname>"`，而 Prometheus 默认 `honor_labels: false`
会让 exporter 的标签覆盖目标地址，所以面板里显示的是主机名。

如果想同时保留目标地址，可以打开 `honor_labels: true`，
此时原地址会被改名为 `exported_instance`：

```yaml
    honor_labels: true
```

## 常用告警示例

```yaml
groups:
  - name: node_exporter
    rules:
      - alert: 主机磁盘SMART异常
        expr: node_disk_smart_health == 0
        for: 5m
        labels: {severity: critical}

      - alert: 硬盘出现坏道倾向
        expr: increase(node_disk_smart_current_pending_sectors_total[1h]) > 0
        for: 10m
        labels: {severity: warning}

      - alert: 磁盘空间不足
        expr: node_filesystem_used_percent > 85
        for: 10m
        labels: {severity: warning}

      - alert: 文件系统变只读
        expr: node_filesystem_readonly == 1
        for: 5m
        labels: {severity: critical}

      - alert: 内存不足
        expr: node_memory_available_percent < 10
        for: 10m
        labels: {severity: warning}

      - alert: 服务异常退出
        expr: node_systemd_unit_state{state="failed"} == 1
        for: 5m
        labels: {severity: warning}

      - alert: 采集器自身异常
        expr: scrape_error > 0
        for: 10m
        labels: {severity: warning}
```

## 依赖说明

| 模块 | 依赖 |
| --- | --- |
| CPU / 内存 / 温度 / 网卡 / 文件系统 | 无（只读 `/proc`、`/sys` 并调用 `statfs`） |
| systemd | `systemctl` |
| SMART | `smartmontools`，读 `/dev/sd*`、`/dev/nvme*` |

可选依赖缺失时不会导致整体抓取失败：对应模块的 `*_scrape_error` 会被置为 1，
其余指标照常输出，启动日志也会给出警告。

## 开发

```bash
make test        # 单元测试
make test-race   # 竞态检测
make cover       # 覆盖率
make lint        # gofmt + go vet
make build       # 编译
```

目录结构：

```
node_exporter.go       入口：参数/配置解析、采集器注册、HTTP 服务、instance 标签改写
instance/              hostname 识别与 instance 标签改写
internal/util/         共用辅助：数值解析、文件读取、带超时的命令执行、正则筛选
cpu/ mem/ hwmon/       基础系统指标
network/ systemd/      流量与 systemd（含筛选）
filesystem/            文件系统容量（statfs）
smart/                 可选模块（默认关闭）
```

## Grafana 面板

面板标题 **机群主机监控**（uid `host-monitor`，15 块）。`grafana/` 下两份是同一套面板，粘的地方不同：

| 文件 | 用在哪 |
| --- | --- |
| `grafana/dashboard.json` | Dashboards → New → **Import**（v1，Grafana 会自动迁移成 v2） |
| `grafana/dashboard-v2.json` | Dashboard settings → **JSON Model**（v2，Grafana 13 的编辑器只认这个格式） |

导入时把 `DS_PROMETHEUS` 绑到自己的 Prometheus 数据源。面板清单：

| # | 面板 | 类型 | PromQL |
| --- | --- | --- | --- |
| 1 | 在线主机 | stat | `count(up{job="node_exporter"})` |
| 2 | 离线目标 | stat | `sum(up{job=~"node_exporter\|zfs_exporter\|lxc_exporter\|bird_exporter\|portfwd_exporter"} == 0) or vector(0)` |
| 3 | 失败的服务单元 | stat | `sum(node_systemd_units_failed{instance=~"$instance"})` |
| 4 | 采集目标状态 | table | `up{job=~"node_exporter\|zfs_exporter\|lxc_exporter\|bird_exporter\|portfwd_exporter"}` |
| 5 | 系统负载（1 分钟） | timeseries | `node_load1{instance=~"$instance"}` |
| 15 | 每核负载（1 / 5 / 15 分钟） | timeseries | `node_load_per_core_ratio{interval="1m",instance=~"$instance"}<br>node_load_per_core_ratio{interval="5m",instance=~"$instance"}<br>node_load_per_core_ratio{interval="15m",instance=~"$instance"}` |
| 6 | CPU 使用率 | timeseries | `(1 - rate(node_cpu_seconds_total{cpu="total",mode="idle",instance=~"$instance"}[$__rate_interval])) * 100` |
| 7 | 内存使用率 | timeseries | `node_memory_used_percent{instance=~"$instance"}` |
| 8 | 内存明细 | timeseries | `node_memory_used_bytes{instance=~"$instance"}<br>node_memory_cached_bytes{instance=~"$instance"}<br>node_memory_buffers_bytes{instance=~"$instance"}` |
| 9 | 磁盘使用率 > 60% | table | `node_filesystem_used_percent{instance=~"$instance"} > 60` |
| 10 | 磁盘可用空间 | timeseries | `node_filesystem_avail_bytes{instance=~"$instance",mountpoint!~"/boot/efi\|/var/tmp\|/run.*"}` |
| 11 | 网卡流量 | timeseries | `sum by (instance) (rate(node_network_receive_bytes_total{instance=~"$instance"}[$__rate_interval]))<br>sum by (instance) (rate(node_network_transmit_bytes_total{instance=~"$instance"}[$__rate_interval]))` |
| 12 | 温度 | timeseries | `node_hwmon_temp_celsius{instance=~"$instance"}<br>node_thermal_zone_temp_celsius{instance=~"$instance"}` |
| 13 | 失败的服务单元明细 | table | `node_systemd_unit_state{state="failed",instance=~"$instance"} == 1` |
| 14 | 硬盘 SMART | table | `node_disk_smart_health<br>node_disk_smart_temperature_celsius<br>node_disk_smart_current_pending_sectors_total<br>node_disk_smart_power_on_hours_total` |

变量：DS_PROMETHEUS、instance。
