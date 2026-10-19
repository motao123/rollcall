package store

import (
	"encoding/json"
	"testing"

	"github.com/motao/rollcall/internal/model"
)

// TestImportIgnoresRevokedSessionsInBackup 是 P1 回归：
//
// ExportConfig 明确不导出 revoked_sessions（运行态安全状态），
// 但 ImportConfig 原本会把备份里的 settings 全量合并 —— 排除规则不对称。
// 一份手工构造（或来自旧版本）的备份带上该键，就能在导入后清空本机吊销表，
// 重启后已登出的旧 Cookie 重新可用。
//
// 注意：已有的 TestConfigExportImportPreservesTokens 只覆盖"正常导出的备份
// 不含该键"这一条路径，因此测不出这个缺口 —— 这里必须手工把键注入备份。
func TestImportIgnoresRevokedSessionsInBackup(t *testing.T) {
	src := New(t.TempDir())
	src.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	raw, err := src.ExportConfig()
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 把吊销表塞回备份（模拟旧版本导出 / 手工构造的文件）
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("备份不是合法 JSON: %v", err)
	}
	settings, ok := doc["settings"].(map[string]any)
	if !ok {
		settings = map[string]any{}
		doc["settings"] = settings
	}
	settings["revoked_sessions"] = `{}`
	crafted, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新编码失败: %v", err)
	}

	dst := New(t.TempDir())
	const local = `{"session-token":"revoked"}`
	dst.SetSetting("revoked_sessions", local)

	if err := dst.ImportConfig(crafted); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if got := dst.GetSetting("revoked_sessions"); got != local {
		t.Fatalf("导入不得覆盖本机会话吊销表：期望 %q，实际 %q（已登出的旧会话会重新可用）", local, got)
	}
	// 其余设置仍应正常导入：只跳过吊销表，不能把整段设置都丢掉。
	dst2 := New(t.TempDir())
	doc["settings"].(map[string]any)["theme"] = `{"mode":"dark"}`
	crafted2, _ := json.Marshal(doc)
	if err := dst2.ImportConfig(crafted2); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if got := dst2.GetSetting("theme"); got != `{"mode":"dark"}` {
		t.Errorf("普通设置应正常导入，实际 %q", got)
	}
}
