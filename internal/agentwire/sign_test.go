package agentwire

import "testing"

// 固定向量测试：canonical 格式一旦被改动（多加一个字段、换分隔符、
// 调整版本前缀），线上所有未升级的 Agent 会开始 401，而且失败现象
// 是"签名对不上"这种最难归因的形态。这条用例的作用就是把格式钉住：
// 想改就必须同时改这里，改动会被看见。
func TestReportSignatureVector(t *testing.T) {
	const (
		token = "secret-token"
		nonce = "00112233445566778899aabbccddeeff"
		want  = "6fbdd2a9e53e51c963acfe5a4200ed5f0df8bdebbd586e94215878380a6e2b6d"
	)
	body := []byte(`{"nodeId":"n1","metrics":{"cpuPct":12.5}}`)
	if got := ReportSignature(token, 1700000000, nonce, body); got != want {
		t.Fatalf("签名向量变了（协议不兼容）：\n want %s\n got  %s", want, got)
	}
	if !VerifyReportSignature(token, 1700000000, nonce, body, want) {
		t.Fatal("自校验失败")
	}
}

// 每一个参与签名的输入都必须真的影响结果，否则等于没签。
func TestReportSignatureCoversInputs(t *testing.T) {
	body := []byte(`{"a":1}`)
	base := ReportSignature("tok", 1700000000, "n", body)
	cases := map[string]string{
		"换令牌":    ReportSignature("tok2", 1700000000, "n", body),
		"换时间":    ReportSignature("tok", 1700000001, "n", body),
		"换nonce": ReportSignature("tok", 1700000000, "m", body),
		"换正文":    ReportSignature("tok", 1700000000, "n", []byte(`{"a":2}`)),
	}
	for name, sig := range cases {
		if sig == base {
			t.Fatalf("%s 不应得到相同签名", name)
		}
	}
	if VerifyReportSignature("tok", 1700000000, "m", body, base) {
		t.Fatal("nonce 不同却验签通过")
	}
}
