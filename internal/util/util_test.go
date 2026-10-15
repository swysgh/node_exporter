package util

import (
	"context"
	"math"
	"testing"
)

func TestParseSizeBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1.41T", 1550311395164, true},
		{"7.21G", 7741678551, true},
		{"512K", 524288, true},
		{"1024", 1024, true},
		{"0", 0, true},
		{"0B", 0, true},
		{"-", 0, false},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSizeBytes(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseSizeBytes(%q) = (%d, %v)，期望 (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTrimNumberSuffix(t *testing.T) {
	cases := map[string]string{
		"30%":    "30",
		"1.00x":  "1.00",
		"0B":     "0",
		"1.41T":  "1.41",
		"7":      "7",
		"1e9":    "1e9",
		"-":      "-",
		"":       "",
		" 42.5":  "42.5",
		"0.05GB": "0.05",
	}
	for in, want := range cases {
		if got := TrimNumberSuffix(in); got != want {
			t.Errorf("TrimNumberSuffix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseNumber(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"30%", 30, true},
		{"1.00x", 1, true},
		{"-", 0, false},
		{"", 0, false},
		{"3", 3, true},
	}
	for _, c := range cases {
		got, ok := ParseNumber(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseNumber(%q) = (%v, %v)，期望 (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFilter(t *testing.T) {
	f, err := NewFilter("", "")
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}
	for _, name := range []string{"eth0", "lo", "br-abc"} {
		if !f.Match(name) {
			t.Errorf("空过滤器应放行 %q", name)
		}
	}

	f, err = NewFilter("^(eth|enp)", "^(eth1|veth)")
	if err != nil {
		t.Fatalf("NewFilter 失败: %v", err)
	}
	cases := map[string]bool{
		"eth0": true, "enp3s0": true, // 匹配 include
		"eth1":  false, // 命中 exclude
		"veth0": false, // 命中 exclude
		"wlan0": false, // 不匹配 include
	}
	for name, want := range cases {
		if got := f.Match(name); got != want {
			t.Errorf("Filter.Match(%q) = %v，期望 %v", name, got, want)
		}
	}

	if _, err := NewFilter("[bad", ""); err == nil {
		t.Error("非法 include 正则应当返回错误")
	}
	if _, err := NewFilter("", "[bad"); err == nil {
		t.Error("非法 exclude 正则应当返回错误")
	}
}

func TestDelta(t *testing.T) {
	// 正常增量：100 / 2 秒 = 50
	if v, ok := Delta(10, 110, 2); !ok || v != 50 {
		t.Errorf("Delta(10,110,2) = (%v,%v)，期望 (50,true)", v, ok)
	}
	// 首次采样没有基线。
	if _, ok := Delta(0, 100, 1); !ok {
		t.Error("计数器回绕时应返回 ok=false")
	}
	// 时间间隔非法。
	if _, ok := Delta(10, 110, 0); ok {
		t.Error("secs=0 时应返回 ok=false")
	}
	if _, ok := Delta(10, 110, -1); ok {
		t.Error("secs<0 时应返回 ok=false")
	}
}

func TestSafeDiv(t *testing.T) {
	if got := SafeDiv(1, 0); got != 0 {
		t.Errorf("SafeDiv(1,0) = %v，期望 0", got)
	}
	if got := SafeDiv(10, 4); got != 2.5 {
		t.Errorf("SafeDiv(10,4) = %v，期望 2.5", got)
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(" /dev/sda , /dev/sdb ,, ")
	want := []string{"/dev/sda", "/dev/sdb"}
	if len(got) != len(want) {
		t.Fatalf("SplitList = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitList[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestRunCommand(t *testing.T) {
	// 正常执行。
	res := RunCommand(context.Background(), "echo", "hello")
	if res.Err != nil {
		t.Fatalf("RunCommand 失败: %v", res.Err)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("Stdout = %q，期望 %q", res.Stdout, "hello\n")
	}

	// 命令不存在：应包装 ErrCommandNotFound。
	res = RunCommand(context.Background(), "definitely-not-a-real-command-xyz")
	if res.Err == nil {
		t.Fatal("命令不存在时应返回错误")
	}

	// 非 0 退出但仍有 stdout：调用方需要能从输出里取信息。
	res = RunCommand(context.Background(), "sh", "-c", "echo partial; exit 2")
	if res.Err == nil {
		t.Error("非 0 退出时应返回错误")
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d，期望 2", res.ExitCode)
	}
	if res.Stdout != "partial\n" {
		t.Errorf("Stdout = %q，期望 %q", res.Stdout, "partial\n")
	}

	// 超时应生效。
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	res = RunCommand(ctx, "sleep", "5")
	if res.Err == nil {
		t.Error("超时的命令应返回错误")
	}
}

func TestParseUint64AndFloat(t *testing.T) {
	if got := ParseUint64(" 42 "); got != 42 {
		t.Errorf("ParseUint64 = %d，期望 42", got)
	}
	if got := ParseUint64("abc"); got != 0 {
		t.Errorf("ParseUint64(非法) = %d，期望 0", got)
	}
	if got := ParseFloat64("1.5"); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("ParseFloat64 = %v，期望 1.5", got)
	}
	if got := ParseInt64("-7"); got != -7 {
		t.Errorf("ParseInt64 = %d，期望 -7", got)
	}
}

func TestSplitLines(t *testing.T) {
	got := SplitLines("a\n\nb\r\n  \nc\n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("SplitLines = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitLines[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}
