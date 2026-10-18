package web

import (
	"encoding/json"
	"net/http"

	"github.com/motao/rollcall/internal/theme"
)

// 主题以受限字段存储：服务端只接受 theme.Theme，未知字段直接拒绝，
// 客户端也只把这些值映射到白名单 CSS 变量，永远不接收 CSS 文本。

func (s *Server) currentTheme() theme.Theme {
	t, err := theme.Parse(s.st.GetSetting("theme"))
	if err != nil {
		return theme.Default()
	}
	return t
}

// handleTheme 公开下发外观配置，供仪表盘与公开状态页应用。
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, s.currentTheme())
}

// handleAdminTheme 读取或保存外观配置。
func (s *Server) handleAdminTheme(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.currentTheme())

	case http.MethodPut, http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 4<<10)
		var in theme.Theme
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if err := in.Validate(); err != nil {
			fail(w, 400, err.Error())
			return
		}
		raw, err := in.Marshal()
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.st.SetSetting("theme", raw)
		writeJSON(w, 200, map[string]any{"saved": true, "theme": in.Normalize()})

	default:
		fail(w, 405, "method not allowed")
	}
}
