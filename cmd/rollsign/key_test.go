package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// TestParsePrivateKeyRejectsMismatchedPublicHalf 是回归测试。
//
// Ed25519 私钥的后 32 字节是公钥部分，Sign 会直接把它写进签名。
// 只校验总长度 64 时，公钥部分被截断或改写的私钥照样能签出清单，
// 但任何持有正确公钥的 Agent 都会验签失败 —— 表现为「更新永远装不上」，
// 而清单本身看起来完全正常。
func TestParsePrivateKeyRejectsMismatchedPublicHalf(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	enc := func(b []byte) []byte {
		return []byte(base64.StdEncoding.EncodeToString(b))
	}
	if _, err := parsePrivateKey(enc(priv)); err != nil {
		t.Fatalf("合法私钥应通过: %v", err)
	}

	// 只破坏公钥部分
	bad := append([]byte(nil), priv...)
	copy(bad[32:], make([]byte, 32))
	if _, err := parsePrivateKey(enc(bad)); err == nil {
		t.Fatal("公私半不一致的私钥必须被拒绝")
	}

	// 长度不足
	if _, err := parsePrivateKey(enc(priv[:63])); err == nil {
		t.Fatal("长度不足的私钥必须被拒绝")
	}
	// 非 base64
	if _, err := parsePrivateKey([]byte("not base64!!")); err == nil {
		t.Fatal("非 base64 内容必须被拒绝")
	}
}

// TestJoinURLEscapesFileName 防止文件名中的空格/`#`/`?` 生成错误下载地址。
// 直接用字符串拼接时，`#` 之后会被浏览器当成片段、`?` 被当成查询串，
// Agent 因此永远下载不到该架构的更新包。
func TestJoinURLEscapesFileName(t *testing.T) {
	cases := []struct{ base, name, want string }{
		{"https://example.com/rel", "a b.bin", "https://example.com/rel/a%20b.bin"},
		{"https://example.com/rel/", "a#b.bin", "https://example.com/rel/a%23b.bin"},
		{"https://example.com/rel", "a?b.bin", "https://example.com/rel/a%3Fb.bin"},
		{"https://example.com/rel", "rollcall-agent", "https://example.com/rel/rollcall-agent"},
	}
	for _, c := range cases {
		got, err := joinURL(c.base, c.name)
		if err != nil {
			t.Fatalf("joinURL(%q,%q): %v", c.base, c.name, err)
		}
		if got != c.want {
			t.Errorf("joinURL(%q,%q) = %q，期望 %q", c.base, c.name, got, c.want)
		}
	}
}
