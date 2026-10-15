package filesystem

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"

	"node_exporter/internal/util"
)

func TestParseMounts(t *testing.T) {
	content := `/dev/sda1 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
/dev/sdb1 /data\040disk xfs rw 0 0
server:/export /mnt/nfs nfs4 ro,vers=4.2 0 0
malformed
`
	mounts := ParseMounts(content)
	if len(mounts) != 4 {
		t.Fatalf("解析到 %d 个挂载点，期望 4: %+v", len(mounts), mounts)
	}
	if mounts[0].Device != "/dev/sda1" || mounts[0].Mountpoint != "/" || mounts[0].Fstype != "ext4" {
		t.Errorf("第一行解析错误: %+v", mounts[0])
	}
	if mounts[0].ReadOnly {
		t.Error("/ 应为可读写")
	}
	// 八进制转义的空格应被还原。
	if mounts[2].Mountpoint != "/data disk" {
		t.Errorf("转义挂载点 = %q，期望 %q", mounts[2].Mountpoint, "/data disk")
	}
	if !mounts[3].ReadOnly {
		t.Error("/mnt/nfs 应为只读")
	}
}

// statfsStub 返回按挂载点预置的 statfs 结果，便于断言具体数值。
func statfsStub(values map[string]unix.Statfs_t, fail map[string]bool) func(string, *unix.Statfs_t) error {
	return func(path string, buf *unix.Statfs_t) error {
		if fail[path] {
			return unix.ENOENT
		}
		v, ok := values[path]
		if !ok {
			return unix.ENOENT
		}
		*buf = v
		return nil
	}
}

// gather 采集并返回「指标名 -> (标签串 -> 值)」。
func gather(t *testing.T, e *Exporter) map[string]map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]map[string]float64)
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			key := ""
			for _, lp := range m.GetLabel() {
				key += lp.GetName() + "=" + lp.GetValue() + ","
			}
			val := 0.0
			switch {
			case m.GetGauge() != nil:
				val = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				val = m.GetCounter().GetValue()
			}
			if out[mf.GetName()] == nil {
				out[mf.GetName()] = map[string]float64{}
			}
			out[mf.GetName()][key] = val
		}
	}
	return out
}

func TestCollectFilesystemMetrics(t *testing.T) {
	values := map[string]unix.Statfs_t{
		"/":          {Bsize: 4096, Blocks: 1000, Bfree: 400, Bavail: 300, Files: 100, Ffree: 40},
		"/data disk": {Bsize: 1024, Blocks: 2000, Bfree: 500, Bavail: 500, Files: 10, Ffree: 5},
		"/mnt/nfs":   {Bsize: 4096, Blocks: 100, Bfree: 50, Bavail: 50, Files: 1, Ffree: 1},
	}
	fstypeFilter, err := util.NewFilter("", DefaultFstypeExclude)
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}
	e := New(Options{
		MountsPath:   "testdata/mounts",
		FstypeFilter: fstypeFilter,
		Statfs:       statfsStub(values, map[string]bool{"/mnt/gone": true}),
	})

	got := gather(t, e)

	rootKey := "device=/dev/sda1,fstype=ext4,mountpoint=/,"
	if v := got["node_filesystem_size_bytes"][rootKey]; v != 4096000 {
		t.Errorf("size = %v，期望 4096000", v)
	}
	if v := got["node_filesystem_free_bytes"][rootKey]; v != 1638400 {
		t.Errorf("free = %v，期望 1638400", v)
	}
	if v := got["node_filesystem_avail_bytes"][rootKey]; v != 1228800 {
		t.Errorf("avail = %v，期望 1228800", v)
	}
	if v := got["node_filesystem_used_bytes"][rootKey]; v != 2457600 {
		t.Errorf("used = %v，期望 2457600", v)
	}
	if v := got["node_filesystem_used_percent"][rootKey]; v != 70 {
		t.Errorf("used_percent = %v，期望 70", v)
	}
	if v := got["node_filesystem_files"][rootKey]; v != 100 {
		t.Errorf("files = %v，期望 100", v)
	}
	if v := got["node_filesystem_files_free"][rootKey]; v != 40 {
		t.Errorf("files_free = %v，期望 40", v)
	}
	if v := got["node_filesystem_readonly"][rootKey]; v != 0 {
		t.Errorf("readonly = %v，期望 0", v)
	}

	// 带空格的挂载点：device 与 mountpoint 都应还原为真实字符。
	diskKey := "device=/dev/sdb1,fstype=xfs,mountpoint=/data disk,"
	if v := got["node_filesystem_size_bytes"][diskKey]; v != 2048000 {
		t.Errorf("xfs size = %v，期望 2048000", v)
	}

	// 只读文件系统。
	nfsKey := "device=server:/export,fstype=nfs4,mountpoint=/mnt/nfs,"
	if v := got["node_filesystem_readonly"][nfsKey]; v != 1 {
		t.Errorf("nfs readonly = %v，期望 1", v)
	}

	// tmpfs 被默认规则排除，不应出现序列。
	for key := range got["node_filesystem_size_bytes"] {
		if key == "device=tmpfs,fstype=tmpfs,mountpoint=/run," {
			t.Error("tmpfs 应被默认 fstype 排除规则过滤")
		}
	}

	// statfs 失败的挂载点被跳过，但整体不算错误。
	if v := got["node_filesystem_scrape_error"][""]; v != 0 {
		t.Errorf("scrape_error = %v，期望 0", v)
	}
	// 导出 3 个（/、/data disk、/mnt/nfs），排除 1 个（tmpfs），跳过 1 个（gone）。
	if v := got["node_filesystem_mounts"][""]; v != 3 {
		t.Errorf("mounts = %v，期望 3", v)
	}
	if v := got["node_filesystem_mounts_filtered"][""]; v != 1 {
		t.Errorf("mounts_filtered = %v，期望 1", v)
	}
}

func TestCollectMissingMountsFile(t *testing.T) {
	e := New(Options{MountsPath: "testdata/does-not-exist"})
	got := gather(t, e)
	if v := got["node_filesystem_scrape_error"][""]; v != 1 {
		t.Errorf("读取挂载表失败时 scrape_error = %v，期望 1", v)
	}
}

func TestMountpointFilter(t *testing.T) {
	values := map[string]unix.Statfs_t{
		"/":          {Bsize: 4096, Blocks: 1000, Bfree: 400, Bavail: 300, Files: 100, Ffree: 40},
		"/data disk": {Bsize: 1024, Blocks: 2000, Bfree: 500, Bavail: 500, Files: 10, Ffree: 5},
		"/mnt/nfs":   {Bsize: 4096, Blocks: 100, Bfree: 50, Bavail: 50, Files: 1, Ffree: 1},
	}
	filter, err := util.NewFilter("^/data", "")
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}
	e := New(Options{
		MountsPath: "testdata/mounts",
		Filter:     filter,
		Statfs:     statfsStub(values, nil),
	})
	got := gather(t, e)
	if _, ok := got["node_filesystem_size_bytes"]["device=/dev/sda1,fstype=ext4,mountpoint=/,"]; ok {
		t.Error("/ 不应通过 ^/data 的 include 规则")
	}
	if _, ok := got["node_filesystem_size_bytes"]["device=/dev/sdb1,fstype=xfs,mountpoint=/data disk,"]; !ok {
		t.Error("/data disk 应通过 include 规则")
	}
}

func TestNewDefaults(t *testing.T) {
	e := New(Options{})
	if e.opts.MountsPath != "/proc/mounts" {
		t.Errorf("MountsPath = %q", e.opts.MountsPath)
	}
	if e.opts.Statfs == nil {
		t.Error("Statfs 应默认使用 unix.Statfs")
	}
}

func TestDescribeCoversMetrics(t *testing.T) {
	e := New(Options{})
	descCh := make(chan *prometheus.Desc, 64)
	e.Describe(descCh)
	close(descCh)
	names := map[string]bool{}
	for d := range descCh {
		names[d.String()] = true
	}
	for _, want := range []string{"node_filesystem_scrape_error", "node_filesystem_scrape_duration_seconds"} {
		found := false
		for s := range names {
			if strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("Describe 缺少 %s", want)
		}
	}
}
