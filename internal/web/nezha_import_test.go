package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

// nezhaFixture 模拟 Nezha v1 管控 API：令牌正确才返回服务器清单。
func nezhaFixture(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/server", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","result":[
			{"name":"nz-jp","note":"备注A","host":{"country_code":"jp","Platform":"linux"}},
			{"name":"已存在"},
			{"name":"  "},
			{"name":"nz-jp"},
			{"name":"nz-second","host":{}}
		]}`))
	})
	return httptest.NewServer(mux)
}

func TestNezhaImportCreatesAndSkips(t *testing.T) {
	srv, h := scanServer(t)
	srv.st.UpsertNode(&model.Node{ID: "n_exist", Name: "已存在"})

	fixture := nezhaFixture(t, "nz-secret-token")
	defer fixture.Close()

	body := `{"url":"` + fixture.URL + `","token":"nz-secret-token"}`
	w := doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha", body, true)
	if w.Code != http.StatusOK {
		t.Fatalf("导入应成功，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Created []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Command string `json:"command"`
		} `json:"created"`
		Skipped []struct {
			Index  int    `json:"index"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Created) != 2 {
		t.Fatalf("应创建 2 台（nz-jp / nz-second），实际 %d: %+v", len(resp.Created), resp.Created)
	}
	if resp.Created[0].Name != "nz-jp" || resp.Created[1].Name != "nz-second" {
		t.Fatalf("创建名单不符: %+v", resp.Created)
	}
	if !strings.Contains(resp.Created[0].Command, "install.sh") {
		t.Fatalf("接入命令应包含 install.sh: %q", resp.Created[0].Command)
	}
	if len(resp.Skipped) != 2 {
		t.Fatalf("重名应跳过 2 台（已存在 + 批内重复），实际 %d: %+v", len(resp.Skipped), resp.Skipped)
	}

	// 国家码映射：nz-jp 的 country_code=jp → JP
	nodes := srv.st.Nodes()
	for _, n := range nodes {
		if n.Name == "nz-jp" && n.Country != "JP" {
			t.Fatalf("country 应映射为 JP，实际 %q", n.Country)
		}
	}

	// Nezha 令牌绝不回显
	if strings.Contains(w.Body.String(), "nz-secret-token") {
		t.Fatal("响应不应包含 Nezha 令牌")
	}

	// 落库核对：节点存在且带独立令牌
	byName := map[string]*model.Node{}
	for _, n := range srv.st.Nodes() {
		byName[n.Name] = n
	}
	if n := byName["nz-jp"]; n == nil || n.Token == "" {
		t.Fatal("nz-jp 应已建档且带令牌")
	}
	if n := byName["nz-jp"]; n.Note != "备注A" {
		t.Fatalf("note 应来自 Nezha 备注，实际 %q", n.Note)
	}
}

func TestNezhaImportBadTokenPropagates(t *testing.T) {
	srv, h := scanServer(t)
	_ = srv
	fixture := nezhaFixture(t, "nz-correct-token")
	defer fixture.Close()

	w := doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha",
		`{"url":"`+fixture.URL+`","token":"wrong-token"}`, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("令牌错误应 502，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "401") {
		t.Fatalf("错误信息应带 Nezha 返回码: %s", w.Body.String())
	}
}

func TestNezhaImportValidatesInput(t *testing.T) {
	srv, h := scanServer(t)
	_ = srv

	// 非 http(s) URL
	w := doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha",
		`{"url":"ftp://example.com","token":"t"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ftp URL 应 400，实际 %d", w.Code)
	}
	// 内嵌凭据
	w = doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha",
		`{"url":"https://user:pass@example.com","token":"t"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("带凭据 URL 应 400，实际 %d", w.Code)
	}
	// 缺 token
	w = doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha",
		`{"url":"https://example.com"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 token 应 400，实际 %d", w.Code)
	}
	// 回环 http 不加 --allow-insecure、非回环加（命令口径与向导一致）
	fixture := nezhaFixture(t, "nz-correct-token")
	defer fixture.Close()
	w = doReq(t, h, http.MethodPost, "/api/admin/nodes/import/nezha",
		`{"url":"`+fixture.URL+`","token":"nz-correct-token"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("导入失败: %d %s", w.Code, w.Body.String())
	}
}
