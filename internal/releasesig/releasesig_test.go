package releasesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	return pub, priv
}

func artifact(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rollcall-server-linux-amd64")
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sign(t *testing.T, path, version string, priv ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := SignFile(path, version, priv)
	if err != nil {
		t.Fatalf("SignFile: %v", err)
	}
	return raw
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := keyPair(t)
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)

	rel, err := VerifyFile(p, raw, pub)
	if err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	if rel.Version != "v1.14.0" || rel.Size != 4096 || len(rel.SHA256) != 64 {
		t.Fatalf("验签结果异常: %+v", rel)
	}
}

func TestVerifyRejectsTamperedArtifact(t *testing.T) {
	pub, priv := keyPair(t)
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[100] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(p, raw, pub); err == nil {
		t.Fatal("内容被篡改后必须验签失败")
	}

	// 追加字节（大小变化）同样必须拒绝。
	p2 := artifact(t, 4096)
	raw2 := sign(t, p2, "v1.14.0", priv)
	f, err := os.OpenFile(p2, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := VerifyFile(p2, raw2, pub); err == nil || !strings.Contains(err.Error(), "大小") {
		t.Fatalf("追加内容后应按大小拒绝，实际 err=%v", err)
	}
}

func TestVerifyRejectsTamperedMetadata(t *testing.T) {
	pub, priv := keyPair(t)
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		edit func(*Envelope)
	}{
		{"版本", func(e *Envelope) { e.Version = "v9.9.9" }},
		{"哈希", func(e *Envelope) { e.SHA256 = strings.Repeat("0", 64) }},
		{"大小", func(e *Envelope) { e.Size++ }},
		{"schema", func(e *Envelope) { e.Schema = 2 }},
	}
	for _, c := range cases {
		e := env
		c.edit(&e)
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(b, pub); err == nil {
			t.Errorf("篡改%s后必须验签失败", c.name)
		}
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	pub, priv := keyPair(t)
	other, _ := keyPair(t)
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)

	if _, err := Verify(raw, other); err == nil {
		t.Fatal("换一把公钥必须验签失败")
	}
	if _, err := Verify(raw, pub[:16]); err == nil {
		t.Fatal("残缺公钥必须被拒绝且不能 panic")
	}
}

func TestVerifyRejectsMalformedEnvelope(t *testing.T) {
	pub, priv := keyPair(t)
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}

	// 未知字段、尾部多余值、错误签名编码、空文件都必须拒绝。
	withExtra := append([]byte(`{"schema":1,"version":"v1.14.0","sha256":"`+env.SHA256+
		`","size":4096,"signature":"`+env.Signature+`","admin":true}`), '\n')
	if _, err := Verify(withExtra, pub); err == nil {
		t.Error("未知字段必须被拒绝")
	}
	if _, err := Verify(append(append([]byte{}, raw...), []byte("\n{}")...), pub); err == nil {
		t.Error("尾部多余 JSON 必须被拒绝")
	}
	e := env
	e.Signature = "not base64!!"
	b, _ := json.Marshal(e)
	if _, err := Verify(b, pub); err == nil {
		t.Error("非法 base64 签名必须被拒绝")
	}
	if _, err := Verify(nil, pub); err == nil {
		t.Error("空签名文件必须被拒绝")
	}
	if _, err := Verify(make([]byte, MaxSignatureSize+1), pub); err == nil {
		t.Error("超长签名文件必须被拒绝")
	}
}

func TestSignRejectsInvalidInputs(t *testing.T) {
	_, priv := keyPair(t)
	small := artifact(t, 16)
	for _, v := range []string{"", "dev", "v1.2.3\nX", "v1.2.3\r\n"} {
		if _, err := SignFile(small, v, priv); err == nil {
			t.Errorf("版本 %q 应被拒绝", v)
		}
	}
	if _, err := SignFile(filepath.Join(t.TempDir(), "missing"), "v1.14.0", priv); err == nil {
		t.Error("不存在的文件应报错")
	}
	if _, err := SignFile(small, "v1.14.0", priv[:32]); err == nil {
		t.Error("残缺私钥应被拒绝")
	}
	// 未去掉的 v 前缀必须原样保留，供升级脚本与 /readyz 精确比对。
	p := artifact(t, 4096)
	raw := sign(t, p, "v1.14.0", priv)
	if !strings.Contains(string(raw), `"version": "v1.14.0"`) {
		t.Fatalf("签名文件应保留 v 前缀: %s", raw)
	}
}

func TestDecodePublicKey(t *testing.T) {
	pub, _ := keyPair(t)
	ok := base64.StdEncoding.EncodeToString(pub)
	if got, err := DecodePublicKey(" \n" + ok + "\n"); err != nil || !got.Equal(pub) {
		t.Fatalf("合法公钥应通过: %v", err)
	}
	for _, s := range []string{"", "not base64!!", base64.StdEncoding.EncodeToString(pub[:16])} {
		if _, err := DecodePublicKey(s); err == nil {
			t.Errorf("非法公钥 %q 应被拒绝", s)
		}
	}
}
