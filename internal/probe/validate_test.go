package probe

import (
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/model"
)

// TestValidateServicePortAndScheme 防止端口越界与 https 类型配 http 目标。
//
// 端口越界能通过 ValidateService 入库，但每次探测都在 dial 阶段失败，
// 用户看到的是与输入无关的「永远失败」；https 类型配 http 目标则让
// CertDays 恒为 -1，证书到期监控静默失效。
func TestValidateServicePortAndScheme(t *testing.T) {
	r := NewRunner(Config{})
	cases := []struct {
		name    string
		sv      model.Service
		wantErr string
	}{
		{"端口越界", model.Service{Type: "http", Target: "https://example.com:70000/"}, "端口"},
		{"端口为零", model.Service{Type: "http", Target: "https://example.com:0/"}, "端口"},
		{"https 类型配 http 目标", model.Service{Type: "https", Target: "http://example.com/"}, "https://"},
		{"http 类型可探测 https 站点", model.Service{Type: "http", Target: "https://example.com/"}, ""},
		{"带用户信息", model.Service{Type: "http", Target: "https://user:pass@example.com/"}, "用户名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := r.ValidateService(c.sv)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("期望错误含 %q，实际 %v", c.wantErr, err)
			}
		})
	}
}

// TestParseTCPTargetCanonicalPort 防止 "+80"/"080" 通过校验后原样下传。
func TestParseTCPTargetCanonicalPort(t *testing.T) {
	got, err := parseTCPTarget("example.com:080")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != "example.com:80" {
		t.Fatalf("端口应规范化为 80，实际 %q", got)
	}
	if _, err := parseTCPTarget("example.com:+80"); err != nil {
		t.Fatalf("+80 应被接受并规范化: %v", err)
	}
}

// TestDoAppliesServiceValidation 是回归测试。
//
// doHTTP/doTCP 各自只做自己关心的检查，而「https 类型必须配 https 目标」
// 「URL 不能内嵌凭据」「端口范围」这些规则只在 ValidateService 里。
// 服务定义还可能来自 state.json（手工编辑、旧版本数据），
// 绕过 API 就直接进入探测循环 —— 运行时必须自己也校验一遍。
func TestDoAppliesServiceValidation(t *testing.T) {
	r := NewRunner(Config{})
	res := r.Do(model.Service{ID: "s1", Type: "https", Target: "http://127.0.0.1:9/"})
	if res.OK {
		t.Fatalf("协议不一致的探测被当作成功: %+v", res)
	}
	if !strings.Contains(res.Detail, "https") {
		t.Fatalf("应按「类型 https 必须配 https 目标」拒绝，实际 detail=%q", res.Detail)
	}

	res = r.Do(model.Service{ID: "s2", Type: "http", Target: "https://user:pass@example.com/"})
	if res.OK || !strings.Contains(res.Detail, "用户名") {
		t.Fatalf("内嵌凭据的 URL 应被拒绝: %+v", res)
	}
}
