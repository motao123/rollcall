package selfupdate

import "testing"

// TestVerifyEnvelopeRejectsBadPublicKeyLength 防止畸形公钥让进程崩溃。
//
// ed25519.Verify 对长度不等于 PublicKeySize 的公钥会直接 panic。
// VerifyEnvelope 是导出入口，Updater.PublicKey 也是导出字段，
// 配置残缺时应当返回错误而不是让 agent 崩溃。
func TestVerifyEnvelopeRejectsBadPublicKeyLength(t *testing.T) {
	_, priv := testKey(t)
	p := payload{
		Version: "1.7.0", PublishedAt: 1700000000,
		Assets: map[string]Asset{
			"linux-amd64": {URL: "https://example.com/a", SHA256: sha([]byte("x")), Size: 1},
		},
	}
	raw := envelopeJSON(t, p, priv)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("公钥长度非法时应返回错误，不应 panic: %v", r)
		}
	}()
	if _, err := VerifyEnvelope(raw, []byte("short")); err == nil {
		t.Fatal("长度非法的公钥应被拒绝")
	}
	if _, err := VerifyEnvelope(raw, nil); err == nil {
		t.Fatal("空公钥应被拒绝")
	}
}
