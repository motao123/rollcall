package sanitize

import "testing"

func TestTextStripsControlAndBidi(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通文本", "web-01", "web-01"},
		{"首尾空白", "  web-01 \t", "web-01"},
		{"换行被剔除", "web-01\nALERT: ok", "web-01ALERT: ok"},
		{"CR 被剔除", "a\rb", "ab"},
		{"NUL 被剔除", "a\x00b", "ab"},
		{"DEL 被剔除", "a\x7fb", "ab"},
		{"C1 NEL 被剔除", "a\u0085b", "ab"},
		// 双向控制符（Trojan Source）：能让文本在编辑器与终端里显示成
		// 与真实字节序不同的样子，而告警处置正是靠人读这些文本做判断。
		{"LRO U+202D", "a\u202Db", "ab"},
		{"RLO U+202E", "a\u202Eb", "ab"},
		{"PDF U+202C", "a\u202Cb", "ab"},
		{"LRI U+2066", "a\u2066b", "ab"},
		{"PDI U+2069", "a\u2069b", "ab"},
		{"RLM U+200F", "a\u200Fb", "ab"},
	}
	for _, c := range cases {
		if got := Text(c.in, 0); got != c.want {
			t.Errorf("%s: Text(%q) = %q, 期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// TestTextKeepsJoiners 钉住"剔除不能过头"：ZWNJ/ZWJ 是波斯语、印地语等
// 文字里正常词的一部分，一并剔除会毁掉这些文字的显示。
func TestTextKeepsJoiners(t *testing.T) {
	for _, in := range []string{
		"a\u200Cb", // U+200C ZWNJ
		"a\u200Db", // U+200D ZWJ
	} {
		if got := Text(in, 0); got != in {
			t.Errorf("Text(%q) = %q，连接符不应被剔除", in, got)
		}
	}
}

// TestTextTruncatesOnRuneBoundary 钉住截断不产出半个字符：
// 半个字符会让下游 UTF-8 解码失败，在日志与 CSV 里变成替换符。
func TestTextTruncatesOnRuneBoundary(t *testing.T) {
	// 每个中文字符 3 字节；限 5 字节应截到 1 个完整字符
	if got := Text("中文字符", 5); got != "中" {
		t.Fatalf("Text(中文字符,5) = %q, 期望 %q", got, "中")
	}
	// 截断发生在裁空白之前，所以结果可能短于上限
	if got := Text("  ab  ", 4); got != "ab" {
		t.Fatalf("Text(\"  ab  \",4) = %q, 期望 %q", got, "ab")
	}
}

func TestHasControl(t *testing.T) {
	if HasControl("normal-01") {
		t.Fatal("普通文本不应被判为含控制字符")
	}
	if !HasControl("a\nb") {
		t.Fatal("换行应被判为含控制字符")
	}
	if !HasControl("a\u202Eb") {
		t.Fatal("RLO 应被判为含控制字符")
	}
	if !HasControl(string([]byte{0xff, 0xfe})) {
		t.Fatal("非法 UTF-8 应被判为含控制字符")
	}
}

// TestIsControlMatchesString 钉住"一处剔除、一处拒绝"是同一个口径：
// 两处若各写一份判定，就会出现"这里算控制字符、那里不算"的分歧。
// 用 String 而不是 Text 比对：Text 会额外裁掉首尾空白，那是另一件事。
func TestIsControlMatchesString(t *testing.T) {
	for r := rune(0); r < 0x3000; r++ {
		if r == 0 { // 空字符串两侧都无意义
			continue
		}
		s := string(r)
		kept := String(s, 0) == s
		passed := !HasControl(s)
		if kept != passed {
			t.Fatalf("U+%04X 口径不一致: String 保留=%v, HasControl 通过=%v", r, kept, passed)
		}
	}
}
