package alert

import "testing"

// TestRuleTemplatesMatchDefaults 保证模板与 DefaultRules 不漂移：
// 两者共用同一批 ID，一旦有人只改了其中一边（例如把磁盘阈值从 85 调到 90），
// 用户"套用模板"得到的规则就会和"首次安装默认规则"不一致。
func TestRuleTemplatesMatchDefaults(t *testing.T) {
	defaults := DefaultRules()
	tpls := RuleTemplates()
	if len(tpls) != len(defaults) {
		t.Fatalf("模板 %d 条，默认规则 %d 条，数量不一致", len(tpls), len(defaults))
	}
	byID := make(map[string]RuleTemplate, len(tpls))
	for _, tp := range tpls {
		if _, dup := byID[tp.ID]; dup {
			t.Fatalf("模板 ID 重复: %s", tp.ID)
		}
		byID[tp.ID] = tp
	}
	for _, r := range defaults {
		tp, ok := byID[r.ID]
		if !ok {
			t.Fatalf("默认规则 %s 没有对应模板", r.ID)
		}
		if tp.Name != r.Name || tp.Metric != r.Metric || tp.Op != r.Op ||
			tp.Threshold != r.Threshold || tp.Duration != r.Duration || tp.Severity != r.Severity {
			t.Errorf("模板 %s 与默认规则不一致: %+v vs %+v", r.ID, tp, r)
		}
		if tp.Desc == "" {
			t.Errorf("模板 %s 缺少说明文案", r.ID)
		}
	}
}

// TestRuleTemplateRule 检查模板转规则时字段完整、默认启用、无定向范围。
func TestRuleTemplateRule(t *testing.T) {
	for _, tp := range RuleTemplates() {
		r := tp.Rule()
		if r.ID != tp.ID || r.Name != tp.Name || r.Metric != tp.Metric ||
			r.Op != tp.Op || r.Threshold != tp.Threshold ||
			r.Duration != tp.Duration || r.Severity != tp.Severity {
			t.Errorf("模板 %s 转规则后字段丢失: %+v", tp.ID, r)
		}
		if !r.Enabled {
			t.Errorf("模板 %s 转规则后应默认启用", tp.ID)
		}
		// 新建规则必须对全部节点生效；带空切片会让部分调用方误判为"定向到空集合"。
		if len(r.NodeIDs) != 0 || len(r.Groups) != 0 || len(r.ServiceIDs) != 0 {
			t.Errorf("模板 %s 不应携带定向范围", tp.ID)
		}
	}
}

// TestTemplateServiceOnly 确认服务类模板被正确标记，
// 否则表单会给 cert_days 这类指标显示「定向节点」——
// 用户选了节点却完全不生效。
func TestTemplateServiceOnly(t *testing.T) {
	svcMetrics := map[string]bool{
		"service_down": true, "latency": true, "loss": true, "jitter": true, "cert_days": true,
	}
	for _, tp := range RuleTemplates() {
		if got, want := tp.ServiceOnly, svcMetrics[tp.Metric]; got != want {
			t.Errorf("模板 %s（metric=%s）ServiceOnly=%v，want %v", tp.ID, tp.Metric, got, want)
		}
	}
}
