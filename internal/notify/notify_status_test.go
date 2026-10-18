package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNotify2xxMissingStatusIsFailure 防止「假成功」。
//
// 企业微信/钉钉/飞书/Bark/Server酱 出错时 HTTP 仍为 200，
// 只有响应体里的 errcode/code 是真结果。若只校验 JSON 是否合法，
// 一个 `{}` 或误路由返回的无关 JSON 会被当成投递成功。
func TestNotify2xxMissingStatusIsFailure(t *testing.T) {
	SetAllowPrivateTargets(true)
	t.Cleanup(func() { SetAllowPrivateTargets(false) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cases := []struct {
		name string
		call func() error
	}{
		{"wecom", func() error {
			return sendWecom(map[string]string{"webhookUrl": srv.URL}, "s", "b")
		}},
		{"dingtalk", func() error {
			return sendDingtalk(map[string]string{"webhookUrl": srv.URL}, "s", "b")
		}},
		{"lark", func() error {
			return sendLark(map[string]string{"webhookUrl": srv.URL}, "s", "b")
		}},
		{"bark", func() error {
			return sendBark(map[string]string{"serverUrl": srv.URL, "deviceKey": "k"}, "s", "b")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("缺状态字段的 2xx 响应必须判为失败")
			}
			if !strings.Contains(err.Error(), "缺少") {
				t.Fatalf("错误信息应说明缺少状态字段: %v", err)
			}
		})
	}
}
