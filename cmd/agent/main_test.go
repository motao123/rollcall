package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigIntervalBounds(t *testing.T) {
	cases := []struct {
		name     string
		interval int
		want     int
		wantErr  bool
	}{
		{"default", 0, 30, false},
		{"negative defaults", -5, 30, false},
		{"30s", 30, 30, false},
		{"max 60s", 60, 60, false},
		{"61s rejected", 61, 0, true},
		{"1h rejected", 3600, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			body := fmt.Sprintf(`{"server":"https://rollcall.example.com","nodeId":"n1","token":"t","interval":%d}`, c.interval)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "60") {
					t.Errorf("错误信息应说明 60 秒上限: %v", err)
				}
				return
			}
			if cfg.Interval != c.want {
				t.Errorf("interval=%d, want %d", cfg.Interval, c.want)
			}
		})
	}
}

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
