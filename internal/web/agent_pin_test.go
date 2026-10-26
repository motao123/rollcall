package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const testPin = "9f2c1b0a4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

// TestAgentTLSPinEndpoint 覆盖指纹接口的两种形态。
//
// 404 与 200 的区分是有意义的：安装脚本靠它判断「服务端没启用 TLS」
// 与「服务端启用了但接口异常」，前者退回 tlsSkipVerify，后者应当告警。
func TestAgentTLSPinEndpoint(t *testing.T) {
	// 未启用 TLS：404，而不是空 200。
	srv := New(publicStore(t), nil, nil, Config{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/tls-pin", nil))
	if rec.Code != 404 {
		t.Fatalf("未配置指纹时应 404，实际 %d", rec.Code)
	}

	// 配置了指纹：返回十六进制 + 换行，且必须公开可达（安装脚本匿名取）。
	srv2 := New(publicStore(t), nil, nil, Config{CertPin: testPin})
	rec2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec2, httptest.NewRequest("GET", "/agent/tls-pin", nil))
	if rec2.Code != 200 {
		t.Fatalf("应为 200，实际 %d：%s", rec2.Code, rec2.Body.String())
	}
	if got := strings.TrimSpace(rec2.Body.String()); got != testPin {
		t.Fatalf("指纹不符: %q", got)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type 应为 text/plain，实际 %q", ct)
	}
}

// TestAgentInstallScriptFetchesPin 确认安装脚本是「运行时取指纹」而不是把
// 某个实例的指纹硬编码进脚本：脚本由服务端常量生成，写死会让换证书后
// 所有新装的 Agent 都连不上。
func TestAgentInstallScriptFetchesPin(t *testing.T) {
	srv := New(publicStore(t), nil, nil, Config{CertPin: testPin})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/agent/install.sh", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"/agent/tls-pin", // 运行时取指纹
		"serverCertPin",  // 写进 agent.json 的字段名
		"CERT_PIN=$(fetch_cert_pin)",
		`"${#raw}" -eq 64`, // 长度校验，避免把错误页当指纹
		"tlsSkipVerify",    // 无指纹时仍退回旧行为
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本缺少 %q", want)
		}
	}
	if strings.Contains(body, testPin) {
		t.Fatal("脚本里不应硬编码任何实例的证书指纹")
	}
}
