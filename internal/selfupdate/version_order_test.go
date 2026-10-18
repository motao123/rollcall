package selfupdate

import "testing"

// TestPrereleaseOrder 验证预发布标识按 semver 比较，而不是字典序。
//
// 字典序下 "alpha.10" < "alpha.2"，会让 1.0.0-alpha.10 被判定为比
// alpha.2 更旧，自动更新永远升不上去。
func TestPrereleaseOrder(t *testing.T) {
	order := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.2",
		"1.0.0-alpha.10",
		"1.0.0-beta",
		"1.0.0-rc.1",
		"1.0.0",
	}
	for i := 0; i+1 < len(order); i++ {
		a, ok := parseVersion(order[i])
		if !ok {
			t.Fatalf("解析失败: %s", order[i])
		}
		b, ok := parseVersion(order[i+1])
		if !ok {
			t.Fatalf("解析失败: %s", order[i+1])
		}
		if a.compare(b) >= 0 {
			t.Errorf("%s 应小于 %s", order[i], order[i+1])
		}
		if b.compare(a) <= 0 {
			t.Errorf("%s 应大于 %s", order[i+1], order[i])
		}
	}
}
