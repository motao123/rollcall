package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

const gib = uint64(1) << 30

// trafficDetail 取节点详情里的流量周期数据。
func trafficDetail(t *testing.T, h http.Handler, id string) model.TrafficUsage {
	t.Helper()
	w := doReq(t, h, http.MethodGet, "/api/nodes/"+id, "", true)
	if w.Code != 200 {
		t.Fatalf("节点详情失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Traffic model.TrafficUsage `json:"traffic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return resp.Traffic
}

// TestNodeTrafficModeAffectsUsage 回归：
// 周期用量曾固定按 rx+tx 合成，商家按单边计费时用量被算成两倍，
// 配额告警会在真实用量一半时就触发。
func TestNodeTrafficModeAffectsUsage(t *testing.T) {
	srv, h := scanServer(t)
	srv.st.UpsertNode(&model.Node{ID: "n1", Name: "n1", Token: "tk", TrafficQuotaGB: 10, TrafficMode: model.TrafficModeRx})

	// 第一条上报只建立基线，不产生增量。
	srv.st.AddMetrics("n1", model.HostInfo{Hostname: "n1"}, model.Metrics{TS: 1000, NetRx: 100, NetTx: 100})
	// 第二条：下行 +2GiB，上行 +1GiB。
	srv.st.AddMetrics("n1", model.HostInfo{Hostname: "n1"}, model.Metrics{TS: 1030, NetRx: 100 + 2*gib, NetTx: 100 + gib})

	tr := trafficDetail(t, h, "n1")
	if tr.UsedRx != 2*gib || tr.UsedTx != gib {
		t.Fatalf("周期收发增量异常: rx=%d tx=%d", tr.UsedRx, tr.UsedTx)
	}
	if tr.Mode != model.TrafficModeRx {
		t.Errorf("mode=%q，want rx", tr.Mode)
	}
	if tr.Used != 2*gib {
		t.Errorf("单边口径用量=%d，want %d（不能把上行也算进去）", tr.Used, 2*gib)
	}

	// 改成只计上行后，同一份收发数据应立即按新口径重算。
	body := `{"name":"n1","trafficQuotaGb":10,"trafficMode":"tx"}`
	if w := doReq(t, h, http.MethodPut, "/api/admin/nodes/n1", body, true); w.Code != 200 {
		t.Fatalf("更新口径失败: %d %s", w.Code, w.Body.String())
	}
	if tr = trafficDetail(t, h, "n1"); tr.Used != gib || tr.Mode != model.TrafficModeTx {
		t.Errorf("切换为 tx 后 used=%d mode=%q，want %d/tx", tr.Used, tr.Mode, gib)
	}

	// 取较大者：下行 2GiB > 上行 1GiB。
	body = `{"name":"n1","trafficQuotaGb":10,"trafficMode":"max"}`
	if w := doReq(t, h, http.MethodPut, "/api/admin/nodes/n1", body, true); w.Code != 200 {
		t.Fatalf("更新口径失败: %d %s", w.Code, w.Body.String())
	}
	if tr = trafficDetail(t, h, "n1"); tr.Used != 2*gib {
		t.Errorf("max 口径 used=%d，want %d", tr.Used, 2*gib)
	}

	// 清空口径回到默认合计。
	body = `{"name":"n1","trafficQuotaGb":10}`
	if w := doReq(t, h, http.MethodPut, "/api/admin/nodes/n1", body, true); w.Code != 200 {
		t.Fatalf("清空口径失败: %d %s", w.Code, w.Body.String())
	}
	if tr = trafficDetail(t, h, "n1"); tr.Used != 3*gib || tr.Mode != model.TrafficModeSum {
		t.Errorf("默认口径 used=%d mode=%q，want %d/sum", tr.Used, tr.Mode, 3*gib)
	}
}

// TestNodeTrafficModeValidation 未知口径必须被拒，
// 否则拼错的大小写或单词会被静默当成默认值，用户以为设置生效了。
func TestNodeTrafficModeValidation(t *testing.T) {
	_, h := scanServer(t)
	// 大小写不在拒绝之列（服务端先 lower 再校验），但拼错的单词必须拒。
	for _, bad := range []string{"both", "rx+tx", "0", "sum tx"} {
		body := `{"name":"n1","trafficMode":"` + bad + `"}`
		if w := doReq(t, h, http.MethodPost, "/api/admin/nodes", body, true); w.Code != 400 {
			t.Errorf("口径 %q 应被拒，实际 %d", bad, w.Code)
		}
	}
	// 大小写混写不算错：服务端 lower 后再校验。
	body := `{"name":"n1","trafficMode":"RX"}`
	w := doReq(t, h, http.MethodPost, "/api/admin/nodes", body, true)
	if w.Code != 201 {
		t.Fatalf("大小写混写应接受: %d %s", w.Code, w.Body.String())
	}
	var n model.Node
	if err := json.Unmarshal(w.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if n.TrafficMode != model.TrafficModeRx {
		t.Errorf("落库口径=%q，want rx", n.TrafficMode)
	}
}

// TestRuleMetaTemplatesUsable 保证 rule-meta 下发的每个模板都能被
// POST /api/admin/rules 接受 —— 模板里的阈值/指标与后端校验规则必须一致，
// 否则用户点"套用"只会得到 400。
func TestRuleMetaTemplatesUsable(t *testing.T) {
	_, h := scanServer(t)
	w := doReq(t, h, http.MethodGet, "/api/admin/rule-meta", "", true)
	if w.Code != 200 {
		t.Fatalf("rule-meta 失败: %d", w.Code)
	}
	var meta struct {
		Templates []struct {
			ID          string  `json:"id"`
			Name        string  `json:"name"`
			Metric      string  `json:"metric"`
			Op          string  `json:"op"`
			Threshold   float64 `json:"threshold"`
			Duration    int     `json:"duration"`
			Severity    string  `json:"severity"`
			ServiceOnly bool    `json:"serviceOnly"`
			Applied     bool    `json:"applied"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Templates) < 5 {
		t.Fatalf("模板数量过少: %d", len(meta.Templates))
	}
	for _, tp := range meta.Templates {
		if tp.Applied {
			t.Errorf("全新实例不应有已应用的模板: %s", tp.ID)
		}
		body, _ := json.Marshal(map[string]any{
			"id": tp.ID, "name": tp.Name, "metric": tp.Metric, "op": tp.Op,
			"threshold": tp.Threshold, "duration": tp.Duration, "severity": tp.Severity,
		})
		res := doReq(t, h, http.MethodPost, "/api/admin/rules", string(body), true)
		if res.Code != 201 {
			t.Fatalf("模板 %s 创建失败: %d %s", tp.ID, res.Code, res.Body.String())
		}
		// 同 ID 再建一次必须 409（补齐逻辑依赖它判断"已存在"）。
		dup := doReq(t, h, http.MethodPost, "/api/admin/rules", string(body), true)
		if dup.Code != 409 {
			t.Errorf("模板 %s 重复创建应 409，实际 %d", tp.ID, dup.Code)
		}
	}

	// 创建完成后，已应用标记要真实反映状态。
	w = doReq(t, h, http.MethodGet, "/api/admin/rule-meta", "", true)
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	for _, tp := range meta.Templates {
		if !tp.Applied {
			t.Errorf("模板 %s 已创建但 applied=false", tp.ID)
		}
	}
}
