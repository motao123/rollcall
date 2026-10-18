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

// TestParseVersionRejectsEmptyPrerelease 是回归测试。
//
// `1.2.3-` 与 `1.2.3-+build` 去掉连字符后预发布标识为空。
// 接受它们会让「有预发布后缀」这一判断成立，排序与升级判定都变得不可预期。
func TestParseVersionRejectsEmptyPrerelease(t *testing.T) {
	for _, bad := range []string{"1.2.3-", "1.2.3-+build", "v1.2.3-"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	for _, good := range []string{"1.2.3", "1.2.3-alpha.1", "1.2.3+build.5"} {
		if _, ok := parseVersion(good); !ok {
			t.Errorf("%q 应被接受", good)
		}
	}
}

// TestNumericPreleaseNoOverflow 是回归测试。
//
// 旧实现把数字段转成 int，并「超过 1e9 就不再累加」：
// 32 位平台上 n*10 会先回绕成负数，截断又让 1000000000 与 10000000000
// 变成同一个值 —— 不同版本被判成相等，自动更新直接卡死。
func TestNumericPreleaseNoOverflow(t *testing.T) {
	cases := []struct{ a, b string }{
		{"1.0.0-1000000000", "1.0.0-10000000000"},
		{"1.0.0-999999999", "1.0.0-1000000000"},
		{"1.0.0-alpha.1000000000", "1.0.0-alpha.10000000000"},
	}
	for _, c := range cases {
		a, ok := parseVersion(c.a)
		if !ok {
			t.Fatalf("解析失败: %s", c.a)
		}
		b, ok := parseVersion(c.b)
		if !ok {
			t.Fatalf("解析失败: %s", c.b)
		}
		if a.compare(b) >= 0 {
			t.Errorf("%s 应小于 %s", c.a, c.b)
		}
	}
	if cmpNumeric("007", "7") != 0 {
		t.Errorf("前导零应视为相等: %d", cmpNumeric("007", "7"))
	}
	if cmpNumeric("10", "9") <= 0 {
		t.Errorf("10 应大于 9")
	}
}
