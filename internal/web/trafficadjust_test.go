package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// TestNodeTrafficAdjustEndpoint 覆盖人工校正本周期流量的接口：
// 正常写入、二选一校验、非法值拒绝，以及校正后详情页读到的是新值。
func TestNodeTrafficAdjustEndpoint(t *testing.T) {
	st := publicStore(t)
	st.UpsertNode(&model.Node{ID: "n1", Name: "机-1", TrafficResetDay: 1, TrafficQuotaGB: 100})
	st.AddMetrics("n1", model.HostInfo{}, model.Metrics{TS: time.Now().Unix(), NetRx: 1000, NetTx: 500})
	const admin = "traffic-test-admin-token-123456"
	h := New(st, nil, nil, Config{AdminToken: admin}).Handler()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes/n1/traffic-adjust", strings.NewReader(body))
		req.Header.Set("X-Admin-Token", admin)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := post(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("两个字段都没给应 400，实际 %d", w.Code)
	}
	if w := post(`{"usedGb":1,"usedBytes":1024}`); w.Code != http.StatusBadRequest {
		t.Fatalf("两个字段同时给应 400，实际 %d", w.Code)
	}
	if w := post(`{"usedGb":-1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("负数应 400，实际 %d", w.Code)
	}
	if w := post(`{"usedGb":99999999}`); w.Code != http.StatusBadRequest {
		t.Fatalf("超上限应 400，实际 %d", w.Code)
	}

	w := post(`{"usedGb":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("正常校正应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Before uint64             `json:"before"`
		After  uint64             `json:"after"`
		Usage  model.TrafficUsage `json:"traffic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.After != 5*1024*1024*1024 || out.Usage.Used != out.After {
		t.Fatalf("校正结果不对: after=%d usage=%d", out.After, out.Usage.Used)
	}

	// 详情接口读到的应是校正后的值
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/n1", nil)
	req.Header.Set("X-Admin-Token", admin)
	dw := httptest.NewRecorder()
	h.ServeHTTP(dw, req)
	if dw.Code != http.StatusOK {
		t.Fatalf("节点详情应 200，实际 %d", dw.Code)
	}
	if !strings.Contains(dw.Body.String(), `"used":5368709120`) {
		t.Fatalf("详情未反映校正后的用量: %s", dw.Body.String()[:200])
	}

	// 未知节点 404
	req = httptest.NewRequest(http.MethodPost, "/api/admin/nodes/nope/traffic-adjust",
		strings.NewReader(`{"usedGb":1}`))
	req.Header.Set("X-Admin-Token", admin)
	req.Header.Set("Content-Type", "application/json")
	nw := httptest.NewRecorder()
	h.ServeHTTP(nw, req)
	if nw.Code != http.StatusNotFound {
		t.Fatalf("未知节点应 404，实际 %d", nw.Code)
	}
}
