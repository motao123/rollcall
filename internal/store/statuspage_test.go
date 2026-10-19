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
	ok := &model.Maintenance{ID: "a", Title: "窗口A", StartAt: 100, EndAt: 200, CreatedAt: 111}
	if err := s.UpsertMaintenance(ok); err != nil {
		t.Fatalf("UpsertMaintenance: %v", err)
	}
	if len(s.Maintenances()) != 1 {
		t.Fatal("应有 1 条")
	}
	// 非法区间必须被拒收
	if err := s.UpsertMaintenance(&model.Maintenance{ID: "b", StartAt: 500, EndAt: 100}); err == nil {
		t.Error("结束早于开始的窗口应返回错误")
	}
	// 空 ID 必须被拒收：否则公开页拿不到可删除的稳定标识
	if err := s.UpsertMaintenance(&model.Maintenance{ID: "  ", StartAt: 100, EndAt: 200}); err == nil {
		t.Error("空 ID 应返回错误")
	}
	if len(s.Maintenances()) != 1 {
		t.Error("非法窗口不应写入")
	}
	// 同 ID 覆盖，但 CreatedAt 必须保留（服务端记账，不随编辑漂移）
	if err := s.UpsertMaintenance(&model.Maintenance{ID: "a", Title: "窗口A改", StartAt: 100, EndAt: 300, CreatedAt: 999}); err != nil {
		t.Fatalf("UpsertMaintenance 覆盖: %v", err)
	}
	list := s.Maintenances()
	if len(list) != 1 || list[0].Title != "窗口A改" || list[0].CreatedAt != 111 {
		t.Errorf("同 ID 应覆盖且保留 CreatedAt，实际 %+v", list)
	}
	if deleted, err := s.DeleteMaintenance("a"); err != nil || !deleted {
		t.Errorf("删除应成功: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := s.DeleteMaintenance("a"); err != nil || deleted {
		t.Errorf("重复删除应返回 false: deleted=%v err=%v", deleted, err)
	}
}

// TestMaintenanceCorruptConfigNotOverwritten 配置损坏时写操作必须失败，
// 而不是把读出来的空列表写回去（那等于静默清空管理员的所有窗口）。
func TestMaintenanceCorruptConfigNotOverwritten(t *testing.T) {
	s := statusStore(t)
	s.SetSetting(keyMaintenance, "{not json")
	if err := s.UpsertMaintenance(&model.Maintenance{ID: "x", Title: "窗口", StartAt: 1, EndAt: 2}); err == nil {
		t.Error("配置损坏时 UpsertMaintenance 应返回错误")
	}
	if deleted, err := s.DeleteMaintenance("x"); err == nil || deleted {
		t.Errorf("配置损坏时 DeleteMaintenance 应返回错误: deleted=%v err=%v", deleted, err)
	}
	if raw := s.GetSetting(keyMaintenance); raw != "{not json" {
		t.Errorf("损坏配置被覆盖: %q", raw)
	}
}

// TestMaintenanceAt 全局窗口与按节点窗口的判定。
func TestMaintenanceAt(t *testing.T) {
	s := statusStore(t)
	s.UpsertMaintenance(&model.Maintenance{ID: "g", Title: "全局", StartAt: 0, EndAt: 100})
	global, nodes := s.MaintenanceAt(50)
	if global == nil || global.Title != "全局" || len(nodes) != 0 {
		t.Errorf("全局窗口判定错误: global=%v nodes=%v", global, nodes)
	}
	s2 := statusStore(t)
	s2.UpsertMaintenance(&model.Maintenance{ID: "p", Title: "局部", NodeIDs: []string{"n1"}, StartAt: 0, EndAt: 100})
	global, nodes = s2.MaintenanceAt(50)
	if global != nil {
		t.Error("不应误判为全局")
	}
	if m, ok := nodes["n1"]; !ok || m.Title != "局部" {
		t.Errorf("n1 应命中局部窗口，实际 %+v", nodes)
	}
}

// TestIncidentRoundTrip 人工事件：非法级别退回 info、同 ID 覆盖、删除。
func TestIncidentRoundTrip(t *testing.T) {
	s := statusStore(t)
	if err := s.UpsertIncident(&model.Incident{ID: "i1", Title: "事件一", Level: "x", StartAt: 100, CreatedAt: 111}); err != nil {
		t.Fatalf("UpsertIncident: %v", err)
	}
	list := s.Incidents()
	if len(list) != 1 || list[0].Level != model.LevelInfo {
		t.Errorf("非法级别应退回 info，实际 %+v", list)
	}
	if err := s.UpsertIncident(&model.Incident{ID: "i1", Title: "事件一改", Level: model.LevelWarn, StartAt: 100, EndAt: 200, CreatedAt: 999}); err != nil {
		t.Fatalf("UpsertIncident 覆盖: %v", err)
	}
	if list = s.Incidents(); len(list) != 1 || list[0].Title != "事件一改" || list[0].CreatedAt != 111 {
		t.Errorf("同 ID 应覆盖且保留 CreatedAt，实际 %+v", list)
	}
	if err := s.UpsertIncident(&model.Incident{ID: "i2", Title: "坏区间", StartAt: 200, EndAt: 100}); err == nil {
		t.Error("结束早于开始的事件应返回错误")
	}
	if err := s.UpsertIncident(&model.Incident{ID: "i3", Title: "  ", StartAt: 100}); err == nil {
		t.Error("空标题应返回错误")
	}
	if len(s.Incidents()) != 1 {
		t.Error("非法事件不应写入")
	}
	if deleted, err := s.DeleteIncident("i1"); err != nil || !deleted {
		t.Errorf("删除应成功: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := s.DeleteIncident("i1"); err != nil || deleted {
		t.Errorf("重复删除应返回 false: deleted=%v err=%v", deleted, err)
	}
}

// TestIncidentCorruptConfigNotOverwritten 与维护窗口同理：损坏数据不能被空列表覆盖。
func TestIncidentCorruptConfigNotOverwritten(t *testing.T) {
	s := statusStore(t)
	s.SetSetting(keyIncidents, "[")
	if err := s.UpsertIncident(&model.Incident{ID: "x", Title: "事件", StartAt: 1}); err == nil {
		t.Error("配置损坏时 UpsertIncident 应返回错误")
	}
	if deleted, err := s.DeleteIncident("x"); err == nil || deleted {
		t.Errorf("配置损坏时 DeleteIncident 应返回错误: deleted=%v err=%v", deleted, err)
	}
	if raw := s.GetSetting(keyIncidents); raw != "[" {
		t.Errorf("损坏配置被覆盖: %q", raw)
	}
}

// TestPublicHandleSaltPersistsImmediately 公开句柄盐值必须同步落盘：
// 走防抖保存时，首次请求后立刻重启会让所有已分享的链接失效。
func TestPublicHandleSaltPersistsImmediately(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	h1 := s.PublicHandle("n1")
	h2 := s.PublicHandle("n2")
	if h1 == h2 {
		t.Fatalf("不同节点应得到不同句柄: %s", h1)
	}

	s2 := New(dir)
	if got := s2.PublicHandle("n1"); got != h1 {
		t.Fatalf("重启后句柄变化: %s -> %s", h1, got)
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
