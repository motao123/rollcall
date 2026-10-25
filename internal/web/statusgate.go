package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// publicStatusCookie 是公开页通过 ?t=<token> 首次访问后种下的凭据，
// 让页面里的 status.js、样式与 SSE 不必在每个请求上重复带查询串。
const publicStatusCookie = "rollcall_st"

// publicGate 给公开页及其全部资源套上"总开关 + 访问令牌"。
//
// 两层门槛的意义不同：
//   - Enabled 是总开关。v1.25.0 起默认开启（产品决策：探针装好就该有
//     状态页，脱敏保证不变）；显式关闭后对匿名一律 404。
//   - Token 是"知道链接才进得来"，不是账号体系；为空表示不校验。
//
// 关闭时一律返回 404 而不是 403：403 等于告诉扫描器"这里确实有个页面"。
func (s *Server) publicGate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.st.StatusPage()
		if !cfg.IsEnabled() {
			publicNotFound(w, r)
			return
		}
		if want := strings.TrimSpace(cfg.Token); want != "" && !s.publicTokenOK(w, r, want) {
			publicNotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// publicNotFound 对 API 路径回 JSON，对页面与静态资源回纯文本 404。
// 前端拿到 HTML 404 会解析失败并弹出无意义的报错，这里保持形状一致。
func publicNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// publicTokenOK 校验访问令牌，命中查询串时顺手种 Cookie。
//
// 只做常量时间比较与形状判断，不做限速：限速会按来源 IP 分桶，
// 这里若也计数，正常的公开页轮询会挤占同一 IP 的鉴权额度。
func (s *Server) publicTokenOK(w http.ResponseWriter, r *http.Request, want string) bool {
	match := func(got string) bool {
		return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
	}

	if got := r.URL.Query().Get("t"); got != "" {
		if !match(got) {
			return false
		}
		// 只有真的对上了才种 Cookie，避免把错误 token 写进浏览器。
		http.SetCookie(w, &http.Cookie{
			Name:     publicStatusCookie,
			Value:    want,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil,
			MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		})
		return true
	}
	if got := r.Header.Get("X-Status-Token"); got != "" {
		return match(got)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return match(strings.TrimPrefix(auth, "Bearer "))
	}
	if c, err := r.Cookie(publicStatusCookie); err == nil {
		return match(c.Value)
	}
	return false
}

// statusGateEnabled 供测试与内部调用判断公开页是否开放。
func (s *Server) statusGateEnabled() bool { return s.st.StatusPage().IsEnabled() }
