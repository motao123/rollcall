package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// seedHistoryNodes 播种两台节点并喂出跨小时的指标，让 1m/5m/1h 聚合都有桶。
func seedHistoryNodes(t *testing.T, st *Store) {
	t.Helper()
	st.UpsertNode(&model.Node{ID: "b1", Name: "b1"})
	st.UpsertNode(&model.Node{ID: "b2", Name: "b2"})
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Minute).Unix()
	ts := base
	for i := 0; i < 60; i++ {
		st.AddMetrics("b1", model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: float64(10 + i%20), NetRx: uint64(1000 + i), NetTx: uint64(500 + i),
			MemUsed: 1 << 30, MemTotal: 4 << 30, DiskUsed: 1 << 30, DiskTotal: 8 << 30,
		})
		st.AddMetrics("b2", model.HostInfo{}, model.Metrics{
			TS: ts, CPUPct: float64(30 - i%10), NetRx: uint64(2000 + i), NetTx: uint64(800 + i),
			MemUsed: 2 << 30, MemTotal: 4 << 30, DiskUsed: 2 << 30, DiskTotal: 8 << 30,
		})
		ts += 300 // 5 分钟一步，60 步跨 5 小时：1m/5m/1h 三层都有桶
	}
}

// TestBackupV3CarriesAndRestoresHistory v3 备份必须携带聚合历史，
// 还原后三层聚合与 lastTS 原样可用；告警记录与队列不在备份内（既有语义）。
func TestBackupV3CarriesAndRestoresHistory(t *testing.T) {
	src := New(t.TempDir())
	seedHistoryNodes(t, src)

	raw, err := src.ExportConfig()
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	var peek struct {
		Version   int    `json:"version"`
		HistoryGz []byte `json:"historyGz"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		t.Fatalf("解析备份失败: %v", err)
	}
	if peek.Version != ConfigBackupVersion || ConfigBackupVersion < 3 {
		t.Fatalf("备份版本应为 %d，实际 %d", ConfigBackupVersion, peek.Version)
	}
	if len(peek.HistoryGz) == 0 {
		t.Fatal("v3 备份应包含 HistoryGz")
	}

	// 还原到一个全新的 store：节点与历史都应来自备份
	dst := New(t.TempDir())
	if err := dst.ImportConfig(raw); err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if got := len(dst.Nodes()); got != 2 {
		t.Fatalf("还原后应有 2 台节点，实际 %d", got)
	}
	// 聚合历史可用：1h 层有桶，且数值来自备份
	for _, id := range []string{"b1", "b2"} {
		pts := dst.HistorySeries(id, "1h", 0)
		if len(pts) == 0 {
			t.Fatalf("节点 %s 还原后 1h 聚合应为空之前的历史, 实际 0 桶", id)
		}
		if pts[0].CPUPct <= 0 {
			t.Fatalf("节点 %s 的 1h CPU 均值应大于 0，实际 %v", id, pts[0].CPUPct)
		}
	}
	// b1 与 b2 的首桶 CPU 不同（10+/30-），证明没有串数据
	p1 := dst.HistorySeries("b1", "1h", 0)[0].CPUPct
	p2 := dst.HistorySeries("b2", "1h", 0)[0].CPUPct
	if p1 == p2 {
		t.Fatalf("两台节点的聚合值不应相同: %v", p1)
	}
}

// TestBackupV2StillImports v2 备份（无 HistoryGz）必须继续可用：
// 还原后节点正常、历史保持还原目标的现状（不受影响）。
func TestBackupV2StillImports(t *testing.T) {
	src := New(t.TempDir())
	src.UpsertNode(&model.Node{ID: "old1", Name: "old1"})
	v2 := []byte(`{"version":2,"savedAt":1700000000,"nodes":[{"id":"old1","name":"old1"}],"tokens":{"old1":"tok"}}`)

	dst := New(t.TempDir())
	dst.UpsertNode(&model.Node{ID: "keep", Name: "keep"})
	seedHistoryNodes(t, dst)
	before := dst.HistorySeries("b1", "1h", 0)

	if err := dst.ImportConfig(v2); err != nil {
		t.Fatalf("v2 备份应可导入: %v", err)
	}
	if got := len(dst.Nodes()); got != 1 || dst.Nodes()[0].ID != "old1" {
		t.Fatalf("v2 导入后节点应来自备份，实际 %+v", dst.Nodes())
	}
	after := dst.HistorySeries("b1", "1h", 0)
	if len(after) != len(before) {
		t.Fatalf("v2 备份不含历史，还原目标的聚合不应被改动: before=%d after=%d", len(before), len(after))
	}
}

// TestBackupHistorySchemaReject 历史快照 schema 高于当前支持时，
// 整个导入必须失败（而不是"配置换掉、历史没动"的半还原）。
func TestBackupHistorySchemaReject(t *testing.T) {
	src := New(t.TempDir())
	seedHistoryNodes(t, src)
	raw, err := src.ExportConfig()
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 把 HistoryGz 换成"未来 schema"的快照
	var cb map[string]any
	if err := json.Unmarshal(raw, &cb); err != nil {
		t.Fatal(err)
	}
	future, _ := json.Marshal(map[string]any{
		"schemaVersion": historySchemaVersion + 1,
		"savedAt":       time.Now().Unix(),
		"nodes":         map[string]any{},
	})
	fz, err := encodeSnapshot(future)
	if err != nil {
		t.Fatal(err)
	}
	cb["historyGz"] = fz
	tampered, _ := json.Marshal(cb)

	dst := New(t.TempDir())
	dst.UpsertNode(&model.Node{ID: "victim", Name: "victim"})
	if err := dst.ImportConfig(tampered); err == nil {
		t.Fatal("schema 高于当前应拒绝导入")
	}
	// 拒绝后状态原样：节点没被换掉
	if got := len(dst.Nodes()); got != 1 || dst.Nodes()[0].ID != "victim" {
		t.Fatalf("拒绝导入后节点应保持原样，实际 %+v", dst.Nodes())
	}
}

// TestExportConfigNoHistoryGzWhenEmpty 无历史（未上报）时导出不带 HistoryGz，
// 还原端据此跳过历史替换而不是清空目标。
func TestExportConfigNoHistoryGzWhenEmpty(t *testing.T) {
	st := New(t.TempDir())
	st.UpsertNode(&model.Node{ID: "only", Name: "only"})
	raw, err := st.ExportConfig()
	if err != nil {
		t.Fatal(err)
	}
	var peek struct {
		HistoryGz []byte `json:"historyGz"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		t.Fatal(err)
	}
	if len(peek.HistoryGz) != 0 {
		t.Fatal("无聚合历史时 HistoryGz 应缺省")
	}
	// 仍可导入到有历史的目标：历史保持原样
	dst := New(t.TempDir())
	seedHistoryNodes(t, dst)
	if err := dst.ImportConfig(raw); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(dst.HistorySeries("b1", "1h", 0)) == 0 {
		t.Fatal("目标已有的聚合历史不应被清空")
	}
}
