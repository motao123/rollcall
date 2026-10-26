package agentwire

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseCertPin 覆盖指纹的正常书写差异与拒绝路径。
//
// 拒绝路径比接受路径重要：截断的指纹若被接受，运行期会变成
// 「永远握手失败」而看不出原因。
func TestParseCertPin(t *testing.T) {
	raw := strings.Repeat("ab", CertPinLen)

	ok := []string{
		raw,
		strings.ToUpper(raw),
		"  " + raw + "  ",
		// 冒号分隔（openssl 风格）与连字符分组
		func() string {
			var parts []string
			for i := 0; i < len(raw); i += 2 {
				parts = append(parts, raw[i:i+2])
			}
			return strings.Join(parts, ":")
		}(),
	}
	for _, s := range ok {
		b, err := ParseCertPin(s)
		if err != nil {
			t.Fatalf("应接受 %q: %v", s, err)
		}
		if len(b) != CertPinLen {
			t.Fatalf("%q 长度应为 %d，实际 %d", s, CertPinLen, len(b))
		}
	}

	bad := []string{
		"",
		"   ",
		raw[:62],       // 少一个字节
		raw + "ab",     // 多一个字节
		"zz" + raw[2:], // 非十六进制
		"ab",           // 太短
	}
	for _, s := range bad {
		if _, err := ParseCertPin(s); err == nil {
			t.Fatalf("应拒绝 %q", s)
		}
	}
}

// TestPinFromCertPEM 确认从 PEM 取到的指纹等于叶证书 DER 的 SHA-256。
func TestPinFromCertPEM(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	cert := srv.Certificate()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	got, err := PinFromCertPEM(pemBytes)
	if err != nil {
		t.Fatalf("取指纹失败: %v", err)
	}
	sum := sha256.Sum256(cert.Raw)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("指纹不一致: got %s want %s", got, want)
	}
	if got != CertPinHex(cert) {
		t.Fatal("PinFromCertPEM 与 CertPinHex 结果不一致")
	}

	// 非证书内容必须拒绝，不能返回空串让调用方当成「无指纹」继续跑。
	if _, err := PinFromCertPEM([]byte("not a pem")); err == nil {
		t.Fatal("非 PEM 应报错")
	}
	if _, err := PinFromCertPEM(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}})); err == nil {
		t.Fatal("非 CERTIFICATE 块应报错")
	}
}

// TestClientTLSConfig 覆盖三种配置的优先级与错误不退化。
func TestClientTLSConfig(t *testing.T) {
	if cfg, err := ClientTLSConfig(false, ""); err != nil || cfg != nil {
		t.Fatalf("无指纹且不跳过校验时应返回 nil 配置: %v %v", cfg, err)
	}
	cfg, err := ClientTLSConfig(true, "")
	if err != nil || cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatalf("仅跳过校验时应返回 InsecureSkipVerify 配置: %v %v", cfg, err)
	}
	pinned, err := ClientTLSConfig(false, strings.Repeat("ab", CertPinLen))
	if err != nil || pinned == nil || !pinned.InsecureSkipVerify || pinned.VerifyPeerCertificate == nil {
		t.Fatalf("固定指纹时应返回带校验回调的配置: %v %v", pinned, err)
	}
	// 关键：写错的指纹必须报错，绝不能退化成「跳过校验」继续跑。
	if _, err := ClientTLSConfig(true, "deadbeef"); err == nil {
		t.Fatal("格式错误的指纹必须报错，不能退化")
	}
}

// TestPinnedTLSHandshake 用真实 TLS 握手验证指纹校验的两种结果。
//
// 只测回调函数不够：如果配置写错（例如漏了 InsecureSkipVerify），
// 自签证书会在 CA 阶段就失败，正确的指纹也连不上 —— 那种情况下
// 「指纹不匹配」的断言同样会通过，测试会假绿。
func TestPinnedTLSHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// 正确指纹：握手成功并拿到响应体。
	good, err := PinFromCertPEM(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err != nil {
		t.Fatalf("取指纹失败: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: mustTLS(t, false, good),
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("指纹正确时应握手成功: %v", err)
	}
	resp.Body.Close()

	// 错误指纹：必须失败，且失败原因是指纹而非 CA。
	wrong := strings.Repeat("00", CertPinLen)
	client2 := &http.Client{Transport: &http.Transport{
		TLSClientConfig: mustTLS(t, false, wrong),
	}}
	if _, err := client2.Get(srv.URL); err == nil {
		t.Fatal("指纹错误时必须拒绝连接")
	} else if !strings.Contains(err.Error(), "指纹不匹配") {
		t.Fatalf("失败原因应指向指纹，实际: %v", err)
	}
}

func mustTLS(t *testing.T, skip bool, pin string) *tls.Config {
	t.Helper()
	c, err := ClientTLSConfig(skip, pin)
	if err != nil {
		t.Fatalf("构造 TLS 配置失败: %v", err)
	}
	return c
}
