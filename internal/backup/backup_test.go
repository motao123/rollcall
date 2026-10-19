package backup

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plain := []byte(`{"version":2,"nodes":[{"id":"n1","token":"secret"}]}`)
	enc, err := Encrypt(plain, "correct horse battery")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if bytes.Contains(enc, plain) {
		t.Fatal("密文中不应出现明文")
	}
	if !strings.HasPrefix(string(enc), magic) {
		t.Fatalf("缺少 magic 头: %q", enc[:5])
	}
	got, err := Decrypt(enc, "correct horse battery")
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("解密结果不一致: %q", got)
	}

	// 同一明文两次加密必须得到不同密文（随机盐与 nonce）。
	enc2, err := Encrypt(plain, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(enc, enc2) {
		t.Error("两次加密结果相同，随机数可能未生效")
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	plain := []byte("config")
	enc, err := Encrypt(plain, "password-123")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Decrypt(enc, "password-124"); err == nil {
		t.Error("错误密码不应解密成功")
	}

	// 翻转密文任意一位必须被 GCM 识别。
	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Decrypt(tampered, "password-123"); err == nil {
		t.Error("密文被篡改仍解密成功")
	}

	// 头部（迭代次数）参与 AAD，篡改后同样失败。
	badIter := append([]byte(nil), enc...)
	binary.BigEndian.PutUint32(badIter[5:9], 300_000)
	if _, err := Decrypt(badIter, "password-123"); err == nil {
		t.Error("迭代次数被篡改仍解密成功")
	}

	for _, in := range [][]byte{
		nil,
		[]byte("short"),
		[]byte("XXXXX" + string(enc[5:])),
		enc[:len(enc)-1],
	} {
		if _, err := Decrypt(in, "password-123"); err == nil {
			t.Errorf("非法输入 %d 字节不应解密成功", len(in))
		}
	}

	// 迭代次数超出允许范围时快速失败，避免恶意文件拖垮 CPU。
	lowIter := append([]byte(nil), enc...)
	binary.BigEndian.PutUint32(lowIter[5:9], 1)
	if _, err := Decrypt(lowIter, "password-123"); err == nil {
		t.Error("过低迭代次数应被拒绝")
	}
	highIter := append([]byte(nil), enc...)
	binary.BigEndian.PutUint32(highIter[5:9], maxIter+1)
	if _, err := Decrypt(highIter, "password-123"); err == nil {
		t.Error("过高迭代次数应被拒绝")
	}
}

func TestEncryptRequiresPassword(t *testing.T) {
	if _, err := Encrypt([]byte("x"), ""); err == nil {
		t.Error("空密码应被拒绝")
	}
}

func TestChecksumStable(t *testing.T) {
	data := []byte("same-bytes")
	a, b := Checksum(data), Checksum(append([]byte(nil), data...))
	if a != b {
		t.Errorf("相同内容校验和不同: %s vs %s", a, b)
	}
	if len(a) != 16 {
		t.Errorf("校验和应为 8 字节十六进制，实际 %q", a)
	}
	if Checksum([]byte("other")) == a {
		t.Error("不同内容校验和不应相同")
	}
}
