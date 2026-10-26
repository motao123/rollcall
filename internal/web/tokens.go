package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// APITokenPrefix 是范围化令牌的固定前缀，便于在日志与密钥扫描中识别。
const APITokenPrefix = "rct_"

type apiTokenRequest struct {
	Name           string   `json:"name"`
	Scopes         []string `json:"scopes"`
	Paths          []string `json:"paths"`
	ExpiresDays    int      `json:"expiresDays"`
	ExpiresMinutes int      `json:"expiresMinutes"`
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
		scopes, ok := normalizeScopes(in.Scopes)
		if !ok {
			fail(w, http.StatusBadRequest, "包含无效的权限范围（仅支持 read / admin）")
			return
		}
		if len(scopes) == 0 {
			fail(w, http.StatusBadRequest, "至少选择一个权限范围")
			return
		}
		paths, ok := normalizeTokenPaths(in.Paths)
		if !ok {
			fail(w, http.StatusBadRequest, "paths 无效：仅支持 /api 下的路径前缀，不含查询串或 ..，最多 20 条")
			return
		}
		if in.ExpiresDays < 0 || in.ExpiresDays > 3650 {
			fail(w, http.StatusBadRequest, "expiresDays 必须在 0–3650 之间（0 表示永不过期）")
			return
		}
		if in.ExpiresMinutes < 0 || in.ExpiresMinutes > 10*365*24*60 {
			fail(w, http.StatusBadRequest, "expiresMinutes 必须在 0–5256000 之间（0 表示永不过期）")
			return
		}
		if in.ExpiresDays > 0 && in.ExpiresMinutes > 0 {
			fail(w, http.StatusBadRequest, "expiresDays 与 expiresMinutes 只能二选一")
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
			Paths:     paths,
			CreatedAt: now,
		}
		ttl := int64(in.ExpiresDays) * 86400
		if in.ExpiresMinutes > 0 {
			ttl = int64(in.ExpiresMinutes) * 60
		}
		if ttl > 0 {
			t.ExpiresAt = now + ttl
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

// normalizeScopes 归一化作用域。第二个返回值为 false 表示请求里
// 含有不支持的作用域 —— 调用方必须整单拒绝。
//
// 旧实现静默丢弃未知项：{"scopes":["read","write"]} 会签发一个只有 read
// 的令牌，调用方以为拿到了写权限，直到某天收到莫名的 401。
func normalizeScopes(in []string) ([]string, bool) {
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
		default:
			return nil, false
		}
	}
	return out, true
}

// normalizeTokenPaths 归一化路径限定。空输入表示不限路径。
// 只接受 /api 下的字面前缀，保持与鉴权中间件的按段边界匹配一致。
func normalizeTokenPaths(in []string) ([]string, bool) {
	if len(in) > 20 {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 200 {
			return nil, false
		}
		if p != "/api" && !strings.HasPrefix(p, "/api/") {
			return nil, false
		}
		if strings.ContainsAny(p, "?#\\ \t\r\n") || strings.Contains(p, "..") || strings.Contains(p, "//") {
			return nil, false
		}
		p = strings.TrimRight(p, "/")
		if p != "/api" && !strings.HasPrefix(p, "/api/") {
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

func newAPIToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return APITokenPrefix + hex.EncodeToString(raw), nil
}

// matchAPIToken 解析并校验 Authorization: Bearer rct_... 令牌。
func (s *Server) matchAPIToken(r *http.Request) (*model.APIToken, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(h[7:])
	if !strings.HasPrefix(raw, APITokenPrefix) {
		return nil, false
	}
	t, ok := s.st.MatchAPIToken(hashAPIToken(raw), time.Now().Unix(), clientIP(r, s.trustedProxies))
	if !ok {
		return nil, false
	}
	// 先清理路径再比对：/api/nodes/n1/../n10 不能因为前缀命中而越界到 n10。
	if !t.AllowsPath(path.Clean(r.URL.Path)) {
		return nil, false
	}
	return t, true
}

// apiTokenOK 校验 Authorization: Bearer rct_... 令牌。
func (s *Server) apiTokenOK(r *http.Request, needAdmin bool) bool {
	t, ok := s.matchAPIToken(r)
	if !ok {
		return false
	}
	if needAdmin && !t.HasScope("admin") {
		return false
	}
	return true
}
