package notify

import "testing"

// TestPluginPasswordFieldIsSecret 防止清单漏写 secret:true 时密钥明文下发。
//
// 这里直接测字段规范化函数，不走插件目录（那需要 Unix 权限语义，
// Windows 上 t.TempDir() 恒为全局可写，会被安全校验拒绝）。
func TestPluginPasswordFieldIsSecret(t *testing.T) {
	f, err := normalizeField(0, Field{
		Key: "token", Label: "令牌", Type: "password", Required: true,
	})
	if err != nil {
		t.Fatalf("合法字段不应报错: %v", err)
	}
	if !f.Secret {
		t.Fatal("type=password 的字段应自动标记 secret")
	}

	f2, err := normalizeField(1, Field{Key: "hook", Label: "地址", Type: "url"})
	if err != nil {
		t.Fatalf("url 字段不应报错: %v", err)
	}
	if f2.Secret {
		t.Fatal("url 字段不应被误标为 secret")
	}

	if _, err := normalizeField(2, Field{Key: "bad key", Label: "x", Type: "text"}); err == nil {
		t.Fatal("非法字段 key 应报错")
	}
}
