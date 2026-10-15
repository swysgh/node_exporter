// Package util 提供各采集模块共用的底层辅助函数：
// 读取 /proc 与 /sys 文件、宽松的数值解析、带单位的大小解析、
// 以及带超时的外部命令执行。
//
// 这些函数全部遵循"永不 panic、永不因解析失败而中断整体采集"的原则：
// 解析失败时返回零值，调用方据此决定是否上报错误指标。
package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrCommandNotFound 表示外部命令不存在（通常是缺少可选依赖）。
var ErrCommandNotFound = errors.New("command not found")

// ReadFile 读取整个文件内容。
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// ReadString 读取整个文件并返回字符串。
func ReadString(path string) (string, error) {
	data, err := ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReadLines 读取文件并按行切分，自动去掉行尾的 \r 与结尾空行。
func ReadLines(path string) ([]string, error) {
	data, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return SplitLines(string(data)), nil
}

// SplitLines 把多行文本切成行列表，跳过空行。
func SplitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// ParseUint64 宽松地把字符串解析为 uint64，失败或为空返回 0。
func ParseUint64(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

// ParseInt64 宽松地把字符串解析为 int64，失败或为空返回 0。
func ParseInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// ParseFloat64 宽松地把字符串解析为 float64，失败或为空返回 0。
func ParseFloat64(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// TrimNumberSuffix 去掉数值末尾的非数字后缀。
//
// 例如 "30%" -> "30"、"1.00x" -> "1.00"、"0B" -> "0"、"1.41T" -> "1.41"。
// 小数点、正负号与合法的 e/E 指数标记（如 1e9）会被保留。
func TrimNumberSuffix(s string) string {
	s = strings.TrimSpace(s)
	end := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isDigit(c), c == '.', c == '-', c == '+':
			end = i + 1
		case (c == 'e' || c == 'E') && end > 0 && i+1 < len(s) &&
			(isDigit(s[i+1]) || s[i+1] == '-' || s[i+1] == '+'):
			end = i + 1
		default:
			// 遇到单位字符，立即结束。
			return s[:end]
		}
	}
	return s[:end]
}

// isDigit 判断字符是否为十进制数字。
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// ParseNumber 解析可能带百分号/单位后缀的数值字符串。
// 例如 "30%" -> 30、"1.00x" -> 1、"-" -> 0（同时 ok=false）。
func ParseNumber(s string) (float64, bool) {
	s = TrimNumberSuffix(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ParseSizeBytes 解析带单位后缀的容量字符串，返回字节数。
//
// 支持 K/M/G/T/P/E（1024 进制）与 B（字节），例如：
//
//	"1.41T" -> 1649267441664
//	"512K"  -> 524288
//	"7.21G" -> 7740047872
//	"1024"  -> 1024（无后缀时按字节处理）
//
// "-" 与空字符串返回 0 与 ok=false。
func ParseSizeBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}

	// 1024 进制单位。
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	case 'P', 'p':
		mult = 1 << 50
	case 'E', 'e':
		mult = 1 << 60
	case 'B', 'b':
		// 字节，倍率保持 1。
	default:
		// 无单位，按字节处理。
	}

	// 去掉一个可能的后缀字符。若之后仍无法解析（例如 "1.41 TiB"），
	// 再退回到"截断非数字后缀"的宽松解析。
	num := s
	if mult != 1 || isSizeUnit(s[len(s)-1]) {
		num = s[:len(s)-1]
	}

	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		v, err = strconv.ParseFloat(TrimNumberSuffix(num), 64)
		if err != nil {
			return 0, false
		}
	}
	return int64(v * float64(mult)), true
}

// isSizeUnit 判断字符是否为 ParseSizeBytes 支持的单位后缀。
func isSizeUnit(c byte) bool {
	switch c {
	case 'K', 'k', 'M', 'm', 'G', 'g', 'T', 't', 'P', 'p', 'E', 'e', 'B', 'b':
		return true
	}
	return false
}

// CommandResult 保存一次外部命令执行的结果。
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// RunCommand 在给定的 context（含超时）内执行外部命令并收集输出。
//
// 即使命令以非 0 退出，也会返回已产生的 stdout/stderr，
// 供调用方从输出中提取信息（例如 smartctl 待机时仍会打印设备信息）。
// 命令不存在时返回包装了 ErrCommandNotFound 的错误。
func RunCommand(ctx context.Context, name string, args ...string) CommandResult {
	cmd := exec.CommandContext(ctx, name, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			res.Err = fmt.Errorf("%s %s: %w (stderr: %s)",
				name, strings.Join(args, " "), err, strings.TrimSpace(res.Stderr))
		} else {
			res.Err = fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				res.Err = fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ErrCommandNotFound)
			}
		}
	}
	return res
}

// LookPath 判断外部命令是否存在于 PATH 中。
func LookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}

// Delta 计算两次累计计数之间的增量与速率。
//
// last 为上一次采样值，cur 为本次采样值，secs 为两次采样的时间间隔（秒）。
// 返回值 ok=false 表示这三种情况之一，不应产出速率指标：
//
//  1. 首次采样（没有上一次的基线）
//  2. 计数器回绕或被重置（cur < last，例如 CPU 重新开机）
//  3. 时间间隔为 0 或非法
func Delta(last, cur uint64, secs float64) (value float64, ok bool) {
	if secs <= 0 || cur < last {
		return 0, false
	}
	return float64(cur-last) / secs, true
}

// SafeDiv 做除法并避免除零，分母为 0 时返回 0。
func SafeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// Filter 是按名称做包含/排除筛选的通用过滤器。
//
// 语义（对每个候选名称 name 求值）：
//
//	include 非空 且 name 不匹配 include  ->  不通过
//	exclude 非空 且 name 匹配 exclude    ->  不通过
//	其余                                 ->  通过
//
// 因此 include 用于"只要某类"，exclude 用于"排除某类"，两者可叠加。
// *regexp.Regexp 本身并发安全，编译一次后可被多个采集协程共享。
type Filter struct {
	Include    *regexp.Regexp
	Exclude    *regexp.Regexp
	includeRaw string
	excludeRaw string
}

// NewFilter 编译包含/排除规则。任一正则非法时返回 error。
func NewFilter(include, exclude string) (*Filter, error) {
	f := &Filter{
		includeRaw: strings.TrimSpace(include),
		excludeRaw: strings.TrimSpace(exclude),
	}

	var err error
	if f.includeRaw != "" {
		if f.Include, err = regexp.Compile(f.includeRaw); err != nil {
			return nil, fmt.Errorf("无效的 include 正则 %q: %w", f.includeRaw, err)
		}
	}
	if f.excludeRaw != "" {
		if f.Exclude, err = regexp.Compile(f.excludeRaw); err != nil {
			return nil, fmt.Errorf("无效的 exclude 正则 %q: %w", f.excludeRaw, err)
		}
	}
	return f, nil
}

// Match 判断名称是否通过筛选。
func (f *Filter) Match(name string) bool {
	if f == nil {
		return true
	}
	if f.Include != nil && !f.Include.MatchString(name) {
		return false
	}
	if f.Exclude != nil && f.Exclude.MatchString(name) {
		return false
	}
	return true
}

// String 返回可读的规则描述，用于启动日志。
func (f *Filter) String() string {
	if f == nil {
		return "include=<全部> exclude=<无>"
	}
	inc, exc := "<全部>", "<无>"
	if f.includeRaw != "" {
		inc = f.includeRaw
	}
	if f.excludeRaw != "" {
		exc = f.excludeRaw
	}
	return fmt.Sprintf("include=%s exclude=%s", inc, exc)
}

// SplitList 把逗号分隔的配置项切分为非空片段列表。
// 用于 -smart.device "/dev/sda,/dev/sdb" 这类参数。
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
