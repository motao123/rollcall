package selfupdate

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCheckELFValidatesClassAndVersion 是回归测试。
//
// 只校验魔数/端序/e_type/machine 时，字长不对（ELFCLASS32）的包能过预检。
// 这种包 exec 会直接失败，而失败发生在 HandleStartup 之前 ——
// 启动计数永远不加，.prev 回滚也永远不会触发。
func TestCheckELFValidatesClassAndVersion(t *testing.T) {
	dir := t.TempDir()
	// fakeELF 生成的是当前架构的合法包头，先确认基线可通过
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, fakeELF("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkELF(ok); err != nil {
		t.Fatalf("合法 ELF 应通过: %v", err)
	}

	wrongClass := byte(1) // ELFCLASS32
	if strconv.IntSize == 32 {
		wrongClass = 2
	}
	cases := []struct {
		name  string
		mut   func([]byte)
		match string
	}{
		{"字长不匹配", func(b []byte) { b[4] = wrongClass }, "字长"},
		{"EI_VERSION 无效", func(b []byte) { b[6] = 0 }, "ELF 版本"},
		{"e_version 无效", func(b []byte) { b[20], b[21], b[22], b[23] = 0, 0, 0, 0 }, "e_version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := fakeELF("v1")
			c.mut(b)
			p := filepath.Join(dir, "bad")
			if err := os.WriteFile(p, b, 0o755); err != nil {
				t.Fatal(err)
			}
			err := checkELF(p)
			if err == nil {
				t.Fatalf("%s 应被拒绝", c.name)
			}
			if !strings.Contains(err.Error(), c.match) {
				t.Fatalf("错误信息未说明原因: %v", err)
			}
		})
	}
}
