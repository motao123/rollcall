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
