package web

import (
	"strings"
	"testing"
)

// 面板 live 轮询的局部更新接线：列表页数据变化时只替换变化的行，
// 结构变化才整页重绘。这些断言防止后续重构把局部路径悄悄删掉，
// 退化成"功能正常但每次上报都整页重建"的性能回退。
func TestLivePatchWiring(t *testing.T) {
	raw, err := assets.ReadFile("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	block := func(name, from, to string) string {
		t.Helper()
		i := strings.Index(src, from)
		if i < 0 {
			t.Fatalf("找不到 %s 的起始标记 %q", name, from)
		}
		rest := src[i:]
		j := strings.Index(rest, to)
		if j <= 0 {
			t.Fatalf("找不到 %s 的结束标记 %q", name, to)
		}
		return rest[:j]
	}

	tick := block("tickLive", "const tickLive = async () => {", "// 节点详情页每 5 秒")
	patchAt := strings.Index(tick, "if (patchLive(ov, nodes)) {")
	if patchAt < 0 {
		t.Fatal("tickLive 必须先尝试 patchLive，再退回 render()")
	}
	renderAt := strings.Index(tick, "render();")
	if renderAt < 0 {
		t.Fatal("tickLive 需要保留整页重绘兜底")
	}
	// 勾选状态存在 DOM 里：整页重绘必须排在局部更新之后、且等取消勾选，
	// 否则 patchLive 的失败分支会清空批量选择。
	if strings.Contains(tick[patchAt:renderAt], "listBusy()") == false {
		t.Fatal("整页重绘兜底前必须检查 listBusy，避免清空批量选择")
	}

	overview := block("viewOverview", "async function viewOverview() {", "async function viewNodes() {")
	nodesView := block("viewNodes", "async function viewNodes() {", "// bindBatchOps 节点多选与批量操作。")
	for name, fn := range map[string]string{"viewOverview": overview, "viewNodes": nodesView} {
		if !strings.Contains(fn, "rememberLiveNodes(nodes);") {
			t.Fatalf("%s 渲染完成后必须记录节点签名，局部更新才知道哪些行是旧的", name)
		}
	}

	patch := block("patchLive", "function patchLive(ov, nodes) {", "async function viewOverview() {")
	if !strings.Contains(patch, "nodesRowCells(n)") || !strings.Contains(patch, "overviewRowCells(n)") {
		t.Fatal("patchLive 必须按页面类型使用对应的行渲染函数")
	}

	overviewCells := block("overviewRowCells", "function overviewRowCells(n) {", "// 节点列表行")
	nodesCells := block("nodesRowCells", "function nodesRowCells(n) {", "function filteredNodes(nodes) {")
	for _, cell := range []string{"state", "name", "region", "os", "cpu", "mem", "disk", "load", "traffic", "uptime", "spark"} {
		if !strings.Contains(overviewCells, `data-cell="`+cell+`"`) {
			t.Fatalf("概览行缺少 data-cell=%q，局部更新会漏掉该列", cell)
		}
	}
	for _, cell := range []string{"state", "name", "provider", "region", "os", "cpu", "mem", "disk", "load", "uptime", "traffic"} {
		if !strings.Contains(nodesCells, `data-cell="`+cell+`"`) {
			t.Fatalf("节点行缺少 data-cell=%q，局部更新会漏掉该列", cell)
		}
	}

	// 筛选态下增删节点可能不改可见行，但「x / y 台」的总数必须跟着变。
	if !strings.Contains(nodesView, "id=\"nodesCountMeta\"") {
		t.Fatal("viewNodes 的节点计数需要稳定 id，局部更新才能刷新总数")
	}
	if !strings.Contains(patch, "nodesCountMeta") {
		t.Fatal("patchLive 必须在可见行不变时也更新节点计数")
	}

	// 勾选列与操作列不能进入 nodesRowCells：局部更新保留这两个单元格，
	// 才能保住批量勾选状态与按钮事件。
	if strings.Contains(nodesCells, "row-sel") || strings.Contains(nodesCells, "data-node-edit") {
		t.Fatal("nodesRowCells 不应包含勾选列或操作列")
	}
	if !strings.Contains(nodesView, "row-sel") || !strings.Contains(nodesView, "data-node-edit") {
		t.Fatal("viewNodes 仍需渲染勾选列与操作列")
	}
}
