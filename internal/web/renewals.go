package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

type renewalView struct {
	model.Renewal
	NodeName string `json:"nodeName"`
}

// handleRenewals 返回续费记录（只读视图）。
func (s *Server) handleRenewals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	nodes := map[string]string{}
	for _, n := range s.st.Nodes() {
		nodes[n.ID] = n.Name
	}
	rows := s.st.Renewals("")
	out := make([]renewalView, 0, len(rows))
	for _, rec := range rows {
		out = append(out, renewalView{Renewal: *rec, NodeName: nodes[rec.NodeID]})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminRenewals 登记一条续费记录。
func (s *Server) handleAdminRenewals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 8<<10)
	var in model.Renewal
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if _, ok := s.st.Node(in.NodeID); !ok {
		fail(w, http.StatusBadRequest, "nodeId 不存在")
		return
	}
	in.At = strings.TrimSpace(in.At)
	if _, err := time.Parse("2006-01-02", in.At); err != nil {
		fail(w, http.StatusBadRequest, "at 必须是 YYYY-MM-DD")
		return
	}
	if in.Amount < 0 || in.Amount > 1e9 {
		fail(w, http.StatusBadRequest, "amount 非法")
		return
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Currency != "USD" && in.Currency != "CNY" {
		fail(w, http.StatusBadRequest, "currency 必须是 USD 或 CNY")
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 200 {
		fail(w, http.StatusBadRequest, "note 过长")
		return
	}
	in.ID = store.NewID("rn")
	in.CreatedAt = time.Now().Unix()
	s.st.AddRenewal(&in)
	writeJSON(w, http.StatusCreated, in)
}

// handleAdminRenewalByID 删除一条续费记录。
func (s *Server) handleAdminRenewalByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/renewals/")
	if id == "" {
		fail(w, http.StatusBadRequest, "missing renewal id")
		return
	}
	if !s.st.DeleteRenewal(id) {
		fail(w, http.StatusNotFound, "renewal not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
