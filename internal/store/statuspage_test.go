package store

import (
	"testing"

	"github.com/motao/rollcall/internal/model"
)

func statusStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

// TestStatusPageDefaultsWhenUnset 未配置时必须返回默认值：
// 零值会把所有区块关掉，公开页变空白。
func TestStatusPageDefaultsWhenUnset(t *testing.T) {
	s := statusStore(t)
	cfg := s.StatusPage()
	def := model.DefaultStatusPage()
	if cfg != def {
		t.Errorf("未配置应返回默认值\n got: %+v\nwant: %+v", cfg, def)
	}
}

// TestStatusPageNormalize 非法输入必须被修正：
// 天数夹回合法档位、空公告整体失效、非法级别退回 info。
func TestStatusPageNormalize(t *testing.T) {
	s := statusStore(t)
	s.SetStatusPage(model.StatusPage{
		Title:        "",
		HistoryDays:  7,
		Announcement: model.StatusAnnouncement{Enabled: true, Level: "bogus", Text: "  "},
	})
	cfg := s.StatusPage()
	if cfg.Title != "服务状态" {
		t.Errorf("空标题应回退默认，实际 %q", cfg.Title)
	}
	if cfg.HistoryDays != 90 {
		t.Errorf("非法天数应回 90，实际 %d", cfg.HistoryDays)
	}
	if cfg.Announcement.Enabled {
		t.Error("空公告必须整体失效")
	}
}

// TestStatusPageKeepsExplicitFalse 显式关闭的字段必须保持关闭。
// 反序列化"缺键保留默认值"的语义不能吃掉管理员主动关闭的开关。
func TestStatusPageKeepsExplicitFalse(t *testing.T) {
	s := statusStore(t)
	cfg := model.DefaultStatusPage()
	cfg.Visibility.Metrics = false
	cfg.Blocks.NodeDetail = false
	s.SetStatusPage(cfg)
	got := s.StatusPage()
	if got.Visibility.Metrics {
		t.Error("显式关闭的 metrics 开关被读回 true")
	}
	if got.Blocks.NodeDetail {
		t.Error("显式关闭的详情入口被读回 true")
	}
}

// TestMaintenanceRoundTrip 窗口的增删改查与非法区间拒绝。
func TestMaintenanceRoundTrip(t *testing.T) {
	s := statusStore(t)
	ok := &model.Maintenance{ID: "a", Title: "窗口A", StartAt: 100, EndAt: 200}
	s.UpsertMaintenance(ok)
	if len(s.Maintenances()) != 1 {
		t.Fatal("应有 1 条")
	}
	// 非法区间必须被拒收
	s.UpsertMaintenance(&model.Maintenance{ID: "b", StartAt: 500, EndAt: 100})
	if len(s.Maintenances()) != 1 {
		t.Error("结束早于开始的窗口不应写入")
	}
	// 同 ID 覆盖
	s.UpsertMaintenance(&model.Maintenance{ID: "a", Title: "窗口A改", StartAt: 100, EndAt: 300})
	list := s.Maintenances()
	if len(list) != 1 || list[0].Title != "窗口A改" {
		t.Errorf("同 ID 应覆盖，实际 %+v", list)
	}
	if !s.DeleteMaintenance("a") {
		t.Error("删除应成功")
	}
	if s.DeleteMaintenance("a") {
		t.Error("重复删除应返回 false")
	}
}

// TestMaintenanceAt 全局窗口与按节点窗口的判定。
func TestMaintenanceAt(t *testing.T) {
	s := statusStore(t)
	s.UpsertMaintenance(&model.Maintenance{ID: "g", Title: "全局", StartAt: 0, EndAt: 100})
	global, nodes := s.MaintenanceAt(50)
	if !global || len(nodes) != 0 {
		t.Errorf("全局窗口判定错误: global=%v nodes=%v", global, nodes)
	}
	s2 := statusStore(t)
	s2.UpsertMaintenance(&model.Maintenance{ID: "p", Title: "局部", NodeIDs: []string{"n1"}, StartAt: 0, EndAt: 100})
	global, nodes = s2.MaintenanceAt(50)
	if global {
		t.Error("不应误判为全局")
	}
	if m, ok := nodes["n1"]; !ok || m.Title != "局部" {
		t.Errorf("n1 应命中局部窗口，实际 %+v", nodes)
	}
}

// TestIncidentRoundTrip 人工事件：非法级别退回 info、同 ID 覆盖、删除。
func TestIncidentRoundTrip(t *testing.T) {
	s := statusStore(t)
	s.UpsertIncident(&model.Incident{ID: "i1", Title: "事件一", Level: "x", StartAt: 100})
	list := s.Incidents()
	if len(list) != 1 || list[0].Level != model.LevelInfo {
		t.Errorf("非法级别应退回 info，实际 %+v", list)
	}
	s.UpsertIncident(&model.Incident{ID: "i1", Title: "事件一改", Level: model.LevelWarn, StartAt: 100, EndAt: 200})
	if list = s.Incidents(); len(list) != 1 || list[0].Title != "事件一改" {
		t.Errorf("同 ID 应覆盖，实际 %+v", list)
	}
	if !s.DeleteIncident("i1") || s.DeleteIncident("i1") {
		t.Error("删除语义错误")
	}
}

// TestPersistenceAcrossRestart 配置与窗口/事件必须随主快照持久化。
// 这类 bug 首次部署测不出来 —— 只有重启才暴露。
func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.SetStatusPage(model.StatusPage{Title: "我的机群", HistoryDays: 30})
	s.UpsertMaintenance(&model.Maintenance{ID: "m", Title: "窗口", StartAt: 1, EndAt: 2})
	s.UpsertIncident(&model.Incident{ID: "i", Title: "事件", StartAt: 1})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := New(dir)
	if cfg := s2.StatusPage(); cfg.Title != "我的机群" || cfg.HistoryDays != 30 {
		t.Errorf("重启后配置丢失: %+v", cfg)
	}
	if len(s2.Maintenances()) != 1 {
		t.Error("重启后维护窗口丢失")
	}
	if len(s2.Incidents()) != 1 {
		t.Error("重启后人工事件丢失")
	}
}
