package store

import (
	"encoding/json"
	"strings"
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

// TestImportRejectsDuplicateAndDanglingRefs 是 P2 回归：
//
// 导入校验原本只查节点 ID 重复，服务/规则重复会被 map 静默覆盖，
// 规则引用的节点/服务也不校验 —— 损坏备份会丢数据或留下永不触发的悬空规则。
func TestImportRejectsDuplicateAndDanglingRefs(t *testing.T) {
	src := New(t.TempDir())
	src.UpsertNode(&model.Node{ID: "n1", Name: "节点一"})
	src.UpsertService(&model.Service{ID: "s1", NodeID: "n1", Name: "Web", Type: "http", Target: "http://127.0.0.1", Enabled: true})
	src.UpsertRule(&model.AlertRule{ID: "r1", Name: "CPU", Metric: "cpu", Op: "gt", Threshold: 90, Enabled: true})
	raw, err := src.ExportConfig()
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	// 构造三类坏备份，逐个断言被拒绝
	mutate := func(fn func(map[string]any)) []byte {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("解析备份失败: %v", err)
		}
		fn(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("编码备份失败: %v", err)
		}
		return out
	}

	cases := []struct {
		name string
		fn   func(map[string]any)
		want string
	}{
		{"服务 ID 重复", func(d map[string]any) {
			svcs := d["services"].([]any)
			d["services"] = append(svcs, svcs[0])
		}, "服务 ID 重复"},
		{"规则 ID 重复", func(d map[string]any) {
			rules := d["rules"].([]any)
			d["rules"] = append(rules, rules[0])
		}, "规则 ID 重复"},
		{"规则引用不存在的节点", func(d map[string]any) {
			r := d["rules"].([]any)[0].(map[string]any)
			r["nodeIds"] = []any{"n_missing"}
		}, "不存在的节点"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := New(t.TempDir())
			err := dst.ImportConfig(mutate(c.fn))
			if err == nil {
				t.Fatalf("应拒绝该备份（%s）", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", c.want, err)
			}
		})
	}
}
