package model

import "testing"

// TestNormalizeSections 覆盖顺序归一化的三条规则：
// 丢弃未知键、去重、补齐缺失键（新增区块不能因老配置而消失）。
func TestNormalizeSections(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"全部合法保持原序", []string{"history", "nodes", "services", "map", "incidents", "activity"},
			[]string{"history", "nodes", "services", "map", "incidents", "activity"}},
		{"未知键被丢弃", []string{"nodes", "bogus", "history"},
			[]string{"nodes", "history", "services", "map", "incidents", "activity"}},
		{"重复键只保留首次", []string{"history", "history", "nodes"},
			[]string{"history", "nodes", "services", "map", "incidents", "activity"}},
		{"空输入回退默认顺序", nil,
			[]string{"nodes", "services", "map", "history", "incidents", "activity"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeSections(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("长度错误: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("顺序错误: got %v want %v", got, c.want)
				}
			}
		})
	}
}
