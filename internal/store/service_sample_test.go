package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// legacySnapshot 是 v1.17.2 及以前的状态文件形态：探测历史存在 svcRes 里，
// 但元素是 Metrics 的复用字段（cpuPct=延迟、tcpConn=成功、procs=证书天数）。
const legacySnapshot = `{
  "schemaVersion": 4,
  "nodes": {},
  "hosts": {},
  "series": {},
  "services": {
    "s1": {"id":"s1","name":"旧目标","type":"https","target":"https://example.com/","interval":60,"enabled":true}
  },
  "svcRes": {
    "s1": [
      {"ts": 100, "cpuPct": 15, "tcpConn": 1, "procs": 30},
      {"ts": 130, "cpuPct": 25, "tcpConn": 1, "procs": 30},
      {"ts": 160, "cpuPct": 0,  "tcpConn": 0, "procs": 30}
    ]
  },
  "rules": {},
  "tokens": {},
  "settings": {}
}`

// TestLoadLegacyServiceSeries 升级后必须能读旧快照里的探测历史。
//
// 存储类型从 Metrics 换成 ServiceSample 时，如果只顾新格式，线上已有的探测历史
// 会在升级那一刻全部变成零值（延迟 0ms、全部失败），看起来像"所有站点同时挂掉"。
func TestLoadLegacyServiceSeries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacySnapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(dir)
	if err := st.load(); err != nil {
		t.Fatalf("加载旧快照失败: %v", err)
	}

	ts, lat, ok, cert, has := st.ServiceLastFull("s1")
	if !has {
		t.Fatal("旧快照的探测历史没被读出来")
	}
	if ts != 160 || lat != 0 || ok || cert != 30 {
		t.Fatalf("最新一点解析错误: ts=%d lat=%d ok=%v cert=%d", ts, lat, ok, cert)
	}

	series := st.ServiceSeries("s1")
	if len(series) != 3 {
		t.Fatalf("历史点数应为 3，实际 %d", len(series))
	}
	// [ts, latency, ok]；第二点延迟 25ms 且成功
	if series[1][2] != 1 || series[1][1] != 25 {
		t.Fatalf("第二点解析错误: %v", series[1])
	}

	// 丢包率与抖动应基于解析后的值算：3 点里 1 点失败 → 33.3%，抖动 |25-15|=10
	st2 := st.ServiceStats("s1", 0)
	if st2.Samples != 3 {
		t.Fatalf("样本数应为 3，实际 %d", st2.Samples)
	}
	if st2.LossPct < 33.2 || st2.LossPct > 33.4 {
		t.Fatalf("丢包率应约 33.3%%，实际 %.2f", st2.LossPct)
	}
	if st2.JitterMs != 10 {
		t.Fatalf("抖动应为 10ms，实际 %.1f", st2.JitterMs)
	}
}

// TestSaveLoadServiceSampleRoundTrip 新格式落盘后重新加载，数值必须一致。
func TestSaveLoadServiceSampleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	st.CreateService(&model.Service{ID: "s1", Name: "新目标", Type: "https",
		Target: "https://example.com/", Interval: 60, Enabled: true})
	st.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: 100, LatencyMs: 15, OK: true, CertDays: 30})
	st.AddServiceResult(model.ServiceResult{ServiceID: "s1", TS: 130, LatencyMs: 0, OK: true, CertDays: 30})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	st2 := New(dir)
	if err := st2.load(); err != nil {
		t.Fatal(err)
	}
	_, lat, ok, cert, has := st2.ServiceLastFull("s1")
	if !has || lat != 0 || !ok || cert != 30 {
		t.Fatalf("往返后最新点不一致: lat=%d ok=%v cert=%d has=%v", lat, ok, cert, has)
	}
	if n := len(st2.ServiceSeries("s1")); n != 2 {
		t.Fatalf("往返后点数应为 2，实际 %d", n)
	}
}
