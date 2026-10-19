package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// APITokenPrefix 是范围化令牌的固定前缀，便于在日志与密钥扫描中识别。
const APITokenPrefix = "rct_"

type apiTokenRequest struct {
	Name        string   `json:"name"`
	Scopes      []string `json:"scopes"`
	ExpiresDays int      `json:"expiresDays"`
}

func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// handleAdminTokens 列出或创建 API 令牌。明文只在创建响应中出现一次。
func (s *Server) handleAdminTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.st.APITokens())
	case http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 8<<10)
		var in apiTokenRequest
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad json: "+err.Error())
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			fail(w, http.StatusBadRequest, "请求包含多余内容")
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len([]rune(in.Name)) > 64 {
			fail(w, http.StatusBadRequest, "name 不能为空且不超过 64 字符")
			return
		}
		scopes := normalizeScopes(in.Scopes)
		if len(scopes) == 0 {
			fail(w, http.StatusBadRequest, "至少选择一个权限范围")
			return
		}
		if in.ExpiresDays < 0 || in.ExpiresDays > 3650 {
			fail(w, http.StatusBadRequest, "expiresDays 必须在 0–3650 之间（0 表示永不过期）")
			return
		}
		raw, err := newAPIToken()
		if err != nil {
			fail(w, http.StatusInternalServerError, "生成令牌失败")
			return
		}
		now := time.Now().Unix()
		t := &model.APIToken{
			ID:        store.NewID("tk"),
			Name:      in.Name,
			Prefix:    raw[:12],
			Hash:      hashAPIToken(raw),
			Scopes:    scopes,
			CreatedAt: now,
		}
		if in.ExpiresDays > 0 {
			t.ExpiresAt = now + int64(in.ExpiresDays)*86400
		}
		s.st.AddAPIToken(t)
		t.Hash = ""
		writeJSON(w, http.StatusCreated, map[string]any{"token": raw, "meta": t})
	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAdminTokenByID 删除令牌。
func (s *Server) handleAdminTokenByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/tokens/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, http.StatusBadRequest, "missing token id")
		return
	}
	if !s.st.DeleteAPIToken(id) {
		fail(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func normalizeScopes(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "read", "admin":
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func newAPIToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return APITokenPrefix + hex.EncodeToString(raw), nil
}

// apiTokenOK 校验 Authorization: Bearer rct_... 令牌。
func (s *Server) apiTokenOK(r *http.Request, needAdmin bool) bool {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return false
	}
	raw := strings.TrimSpace(h[7:])
	if !strings.HasPrefix(raw, APITokenPrefix) {
		return false
	}
	t, ok := s.st.MatchAPIToken(hashAPIToken(raw), time.Now().Unix())
	if !ok {
		return false
	}
	if needAdmin && !t.HasScope("admin") {
		return false
	}
	return true
}
