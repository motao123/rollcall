package collect

import (
	"os"
	"path/filepath"
	"testing"
)

// mkFakeHwmon 造一个假的 /sys/class/hwmon 结构。
func mkFakeHwmon(t *testing.T, sensors map[string]int64) string {
	t.Helper()
	root := t.TempDir()
	for rel, milli := range sensors {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(itoa(milli)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestReadTempCFrom 覆盖 hwmon 读取的三类现实情况：
// 正常多传感器取最高、恒为 0 的伪传感器被忽略、坏读数被跳过。
// （本机群实测：13 台 VPS 全部没有温度传感器 —— 空目录/不存在返回 0
// 必须与"有数据"严格区分，否则 UI 会出现一根贴地的假温度线。）
func TestReadTempCFrom(t *testing.T) {
	// 正常：coretemp 85℃ 是最热的
	root := mkFakeHwmon(t, map[string]int64{
		"hwmon0/temp1_input": 45000,
		"hwmon1/temp1_input": 85000,
		"hwmon1/temp2_input": 78000,
	})
	if got := readTempCFrom(root); got != 85 {
		t.Fatalf("多传感器应取最高温 85，实际 %.1f", got)
	}

	// 恒为 0 的伪传感器 + 一个真实读数：0 不参与
	root2 := mkFakeHwmon(t, map[string]int64{
		"hwmon0/temp1_input": 0,
		"hwmon1/temp1_input": 62000,
	})
	if got := readTempCFrom(root2); got != 62 {
		t.Fatalf("0℃ 伪传感器应被忽略，实际 %.1f", got)
	}

	// 全是 0 / 坏读数 / 目录不存在：一律 0（= 无数据）
	root3 := mkFakeHwmon(t, map[string]int64{"hwmon0/temp1_input": 0})
	if got := readTempCFrom(root3); got != 0 {
		t.Fatalf("全 0 应返回 0，实际 %.1f", got)
	}
	if got := readTempCFrom(filepath.Join(root, "nonexistent")); got != 0 {
		t.Fatalf("不存在的目录应返回 0，实际 %.1f", got)
	}

	// 负读数（少数传感器会报负值）：不参与最高温
	root4 := mkFakeHwmon(t, map[string]int64{"hwmon0/temp1_input": -3000})
	if got := readTempCFrom(root4); got != 0 {
		t.Fatalf("负读数不应成为最高温，实际 %.1f", got)
	}
}

// TestHasHwmonTempIn 覆盖能力探测：有 temp*_input 即认为本机具备温度采集条件。
//
// 判据刻意不用"这次读数是否 >0"：虚拟化环境里的传感器可能恒返回 0
// （readTempCFrom 会把它当无效值丢弃），但文件存在说明机器确实有传感器，
// 不该被上报成"没有传感器"——那会让面板给出错误结论。
func TestHasHwmonTempIn(t *testing.T) {
	// 有传感器（读数恒 0 也算有）
	withSensor := mkFakeHwmon(t, map[string]int64{
		"hwmon0/temp1_input": 0,
	})
	if !hasHwmonTempIn(withSensor) {
		t.Error("存在 temp*_input 时应判定为有传感器")
	}
	// 只有风扇/电压等非温度输入
	other := mkFakeHwmon(t, map[string]int64{
		"hwmon0/fan1_input": 1200,
		"hwmon0/in0_input":  3300,
	})
	if hasHwmonTempIn(other) {
		t.Error("没有 temp*_input 时不应判定为有传感器")
	}
	// 空目录与不存在的目录
	if hasHwmonTempIn(t.TempDir()) {
		t.Error("空目录不应判定为有传感器")
	}
	if hasHwmonTempIn(filepath.Join(t.TempDir(), "nope")) {
		t.Error("不存在的目录不应判定为有传感器")
	}
}

// TestDetectCapsShape 只校验返回值的形态：真实机器上有无传感器取决于硬件，
// 断言具体内容会让测试在开发机（通常有传感器）与 VPS（通常没有）上结果不同。
func TestDetectCapsShape(t *testing.T) {
	caps := DetectCaps()
	if len(caps) > 1 {
		t.Fatalf("当前只定义了一种可选能力，实际返回 %v", caps)
	}
	if len(caps) == 1 && caps[0] != "temp" {
		t.Fatalf("未知能力项: %v", caps)
	}
}
