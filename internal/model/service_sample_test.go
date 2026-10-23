package model

import (
	"encoding/json"
	"testing"
)

// TestServiceSampleRoundTrip 新格式的编解码往返。
func TestServiceSampleRoundTrip(t *testing.T) {
	in := ServiceSample{TS: 1700000000, LatencyMs: 42, OK: true, CertDays: 30}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ServiceSample
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("往返不一致: want %+v got %+v (json=%s)", in, out, b)
	}
	// 0 延迟是合法值（同机探测），必须原样保存而不是被当成缺省
	zero := ServiceSample{TS: 1, LatencyMs: 0, OK: true, CertDays: -1}
	b, _ = json.Marshal(zero)
	var z ServiceSample
	if err := json.Unmarshal(b, &z); err != nil {
		t.Fatal(err)
	}
	if z.LatencyMs != 0 || z.CertDays != -1 || !z.OK {
		t.Fatalf("零值样本被破坏: %s -> %+v", b, z)
	}
}

// TestServiceSampleLegacyFormat 旧快照（探测结果塞在 Metrics 字段里）必须能读。
//
// v1.17.2 及以前的 svcRes 形如 {"ts":…,"cpuPct":15,"tcpConn":1,"procs":30}。
// 升级不能因为换了存储类型就把探测历史丢光，也不能把 0 延迟的旧样本判成空样本。
func TestServiceSampleLegacyFormat(t *testing.T) {
	cases := []struct {
		raw  string
		want ServiceSample
	}{
		{`{"ts":100,"cpuPct":15.6,"tcpConn":1,"procs":30}`, ServiceSample{TS: 100, LatencyMs: 15, OK: true, CertDays: 30}},
		{`{"ts":101,"cpuPct":0,"tcpConn":1,"procs":-1}`, ServiceSample{TS: 101, LatencyMs: 0, OK: true, CertDays: -1}},
		{`{"ts":102,"cpuPct":0,"tcpConn":0,"procs":0}`, ServiceSample{TS: 102, LatencyMs: 0, OK: false, CertDays: 0}},
	}
	for _, c := range cases {
		var got ServiceSample
		if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
			t.Fatalf("%s 解析失败: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("%s\n want %+v\n got  %+v", c.raw, c.want, got)
		}
	}
}
