package web

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/motao123/rollcall/internal/theme"
	"github.com/motao123/rollcall/internal/themepack"
)

// 主题以受限字段存储：服务端只接受 theme.Theme，未知字段直接拒绝，
// 客户端也只把这些值映射到白名单 CSS 变量，永远不接收 CSS 文本。
//
// 在此之外，公开状态页还支持"主题包"：管理员把目录放进 <data>/themes/<name>/，
// 由服务端原样下发其中的 status.css / status.js。主题脚本与站点同源，
// 安装主题等价于授予管理员权限，因此只能使用可信来源。

// themePackSetting 保存当前公开状态页主题包名；空值表示内置外观。
const themePackSetting = "statusTheme"

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

// activeThemePack 返回当前选择且仍通过校验的主题包。
func (s *Server) activeThemePack() (themepack.Pack, error) {
	name := strings.TrimSpace(s.st.GetSetting(themePackSetting))
	if name == "" {
		return themepack.Pack{}, fmt.Errorf("未选择主题包")
	}
	if s.themeDir == "" {
		return themepack.Pack{}, fmt.Errorf("未配置数据目录")
	}
	return themepack.Load(s.themeDir, name)
}

// themePackPage 把主题包资源注入公开状态页外壳。
// 链接与脚本都放在内置资源之后，便于覆盖默认样式与行为。
func themePackPage(page string, p themepack.Pack) string {
	name := html.EscapeString(p.Name)
	page = strings.Replace(page, `<html lang="zh-CN">`,
		`<html lang="zh-CN" data-theme-pack="`+name+`">`, 1)
	if p.HasCSS {
		page = strings.Replace(page, "</head>",
			`<link rel="stylesheet" href="/themes/`+name+`/`+themepack.CSSName+`">`+"\n</head>", 1)
	}
	if p.HasJS {
		page = strings.Replace(page, `<script src="/status.js"></script>`,
			`<script src="/status.js"></script>`+"\n"+
				`<script src="/themes/`+name+`/`+themepack.JSName+`"></script>`, 1)
	}
	return page
}

// handleStatusThemeAsset 下发主题包中的固定资源文件。
//
// 主题名先按严格规则校验（小写字母/数字/连字符），文件名再做白名单，
// 因此请求参数无法拼出 themes 目录之外的路径；符号链接在 themepack.Load
// 中已被拒绝。资源允许缓存但必须每次回源校验，便于管理员直接替换文件。
func (s *Server) handleStatusThemeAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/themes/"), "/")
	if !ok || !themepack.ValidName(name) ||
		(file != themepack.CSSName && file != themepack.JSName) {
		http.NotFound(w, r)
		return
	}
	if s.themeDir == "" {
		http.NotFound(w, r)
		return
	}
	if _, err := themepack.Load(s.themeDir, name); err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.themeDir, name, file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > themepack.MaxAsset {
		http.NotFound(w, r)
		return
	}
	if file == themepack.CSSName {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, file, fi.ModTime(), f)
}

// themePackView 是管理端列表中的单个主题包（含校验失败原因）。
type themePackView struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Author      string `json:"author,omitempty"`
	HasCSS      bool   `json:"hasCss"`
	HasJS       bool   `json:"hasJs"`
	Error       string `json:"error,omitempty"`
}

// handleAdminThemePacks 列出目录中的主题包，或切换公开状态页使用的主题包。
func (s *Server) handleAdminThemePacks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		active := strings.TrimSpace(s.st.GetSetting(themePackSetting))
		out := struct {
			Active  string          `json:"active"`
			Dir     string          `json:"dir"`
			Packs   []themePackView `json:"packs"`
			Warning string          `json:"warning,omitempty"`
		}{Active: active, Dir: s.themeDir, Packs: []themePackView{}}
		for _, e := range themepack.Discover(s.themeDir) {
			v := themePackView{Name: e.Name}
			if e.Err != nil {
				v.Error = e.Err.Error()
			} else {
				v.Title = e.Pack.Title
				v.Description = e.Pack.Description
				v.Version = e.Pack.Version
				v.Author = e.Pack.Author
				v.HasCSS = e.Pack.HasCSS
				v.HasJS = e.Pack.HasJS
			}
			out.Packs = append(out.Packs, v)
		}
		if active != "" {
			if _, err := s.activeThemePack(); err != nil {
				out.Warning = fmt.Sprintf("已选择的主题包 %q 不可用，公开状态页已回退为内置主题：%v", active, err)
			}
		}
		writeJSON(w, 200, out)

	case http.MethodPut, http.MethodPost:
		body := http.MaxBytesReader(w, r.Body, 1<<10)
		var in *struct {
			Name string `json:"name"`
		}
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		if in == nil {
			fail(w, 400, "请求体不能为 null")
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			fail(w, 400, "bad json: 只接受单个 JSON 对象")
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			s.st.SetSetting(themePackSetting, "")
			writeJSON(w, 200, map[string]any{"saved": true, "name": ""})
			return
		}
		if _, err := themepack.Load(s.themeDir, name); err != nil {
			// 选择时就要失败：否则管理员以为已启用，公开页却悄悄回退。
			fail(w, 400, err.Error())
			return
		}
		s.st.SetSetting(themePackSetting, name)
		writeJSON(w, 200, map[string]any{"saved": true, "name": name})

	default:
		fail(w, 405, "method not allowed")
	}
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
