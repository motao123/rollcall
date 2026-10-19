package web

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/motao/rollcall/internal/theme"
)

// 主题以受限字段存储：服务端只接受 theme.Theme，未知字段直接拒绝，
// 客户端也只把这些值映射到白名单 CSS 变量，永远不接收 CSS 文本。

func (s *Server) currentTheme() theme.Theme {
	t, _ := s.themeWithErr()
	return t
}

// themeWithErr 同时返回已保存主题的解析错误。
//
// 公开路径（/api/theme）必须静默回退到默认主题，否则前端拿不到样式；
// 但管理端需要知道「保存的主题坏了」——原实现把所有错误折叠成 Default()，
// 管理员只能看到一套「莫名其妙变回默认」的外观，没有任何线索。
func (s *Server) themeWithErr() (theme.Theme, error) {
	t, err := theme.Parse(s.st.GetSetting("theme"))
	if err != nil {
		return theme.Default(), err
	}
	return t, nil
}

// handleTheme 公开下发外观配置，供仪表盘与公开状态页应用。
// 与其他公开只读接口一致：接受 HEAD 并返回 Allow 头。
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, 200, s.currentTheme())
}

// handleAdminTheme 读取或保存外观配置。
func (s *Server) handleAdminTheme(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		t, parseErr := s.themeWithErr()
		// 保持与之前相同的扁平结构（前端直接读 .mode/.accent/…），
		// 只额外附加一个 warning 字段。
		blob, _ := json.Marshal(t)
		out := map[string]any{}
		_ = json.Unmarshal(blob, &out)
		if parseErr != nil {
			out["warning"] = "已保存的外观配置损坏，已回退为默认值：" + parseErr.Error()
		}
		writeJSON(w, 200, out)

	case http.MethodPut, http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 4<<10)
		var in *theme.Theme
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if in == nil {
			fail(w, 400, "主题配置不能为 null")
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			fail(w, 400, "bad json: 只接受单个 JSON 对象")
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
