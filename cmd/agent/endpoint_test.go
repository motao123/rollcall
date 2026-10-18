package main

import (
	"strings"
	"testing"
)

// TestReportEndpointRejectsEmptyHostname 是回归测试。
//
// 只检查 u.Host != "" 时，`https://:443` 能通过校验：Host 是 ":443"，
// 主机名为空，归一化后得到一个指向本机高位端口的地址，
// 节点令牌会被发到错误的地方。
func TestReportEndpointRejectsEmptyHostname(t *testing.T) {
	for _, bad := range []string{"https://:443", "http://:8080", "https://"} {
		if _, err := reportEndpoint(bad, true); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	got, err := reportEndpoint("https://monitor.example.com:8443/", false)
	if err != nil {
		t.Fatalf("合法地址应通过: %v", err)
	}
	if got != "https://monitor.example.com:8443/api/report" {
		t.Fatalf("归一化结果 = %q", got)
	}
}

// TestReportEndpointRedactsCredentials 防止错误信息把 URL 里的凭据写进日志。
// 这些错误会经 log.Print 打到 stderr，并被运维贴进工单。
func TestReportEndpointRedactsCredentials(t *testing.T) {
	_, err := reportEndpoint("https://alice:s3cr3t@monitor.example.com", false)
	if err == nil {
		t.Fatal("带用户信息的地址应被拒绝")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("错误信息泄露密码: %v", err)
	}
	if _, err := reportEndpoint("https://alice:s3cr3t@monitor.example.com/path", false); err == nil ||
		strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("路径场景同样不应泄露密码: %v", err)
	}
	if got := redactServer("garbage://user:pass@host"); strings.Contains(got, "pass") {
		t.Fatalf("无法解析的地址也应脱敏: %q", got)
	}
}
