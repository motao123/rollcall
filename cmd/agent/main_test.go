package main

import (
	"strings"
	"testing"
)

func TestReportEndpoint(t *testing.T) {
	cases := []struct {
		name          string
		server        string
		allowInsecure bool
		wantErr       bool
	}{
		{"https", "https://rollcall.example.com", false, false},
		{"https trailing slash", "https://rollcall.example.com/", false, false},
		{"loopback http", "http://127.0.0.1:9080", false, false},
		{"localhost http", "http://localhost:9080", false, false},
		{"public http rejected", "http://rollcall.example.com", false, true},
		{"public http allowed explicitly", "http://rollcall.example.com", true, false},
		{"bad scheme", "ftp://rollcall.example.com", false, true},
		{"missing host", "https://", false, true},
	}
	for _, c := range cases {
		got, err := reportEndpoint(c.server, c.allowInsecure)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if err == nil && !strings.HasSuffix(got, "/api/report") {
			t.Errorf("%s: endpoint=%q 缺少 /api/report", c.name, got)
		}
	}
}
