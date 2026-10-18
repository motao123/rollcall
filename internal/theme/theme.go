// Package theme 定义受限的外观主题。
//
// 主题永远不会作为 CSS 文本下发：服务端只接受一组固定字段，
// 客户端也只把它们映射到白名单 CSS 变量，避免主题编辑器变成 XSS 入口。
package theme

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Theme 是允许运营配置的全部外观字段。
type Theme struct {
	Mode       string `json:"mode"`       // auto | light | dark
	Accent     string `json:"accent"`     // 浅色主题强调色，#rrggbb
	AccentDark string `json:"accentDark"` // 深色主题强调色，#rrggbb
	Radius     *int   `json:"radius"`     // 圆角像素 0–24；nil=默认 10
	Compact    bool   `json:"compact"`    // 紧凑布局
}

// Default 返回默认主题（与内置样式一致）。
func Default() Theme {
	return Theme{Mode: "auto"}
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate 严格校验主题字段。未知/越界值必须拒绝而不是静默回退，
// 否则运营会以为保存成功，实际看到的是另一套样式。
func (t Theme) Validate() error {
	switch t.Mode {
	case "", "auto", "light", "dark":
	default:
		return fmt.Errorf("mode 必须是 auto/light/dark")
	}
	// 用有序切片而不是 map：两个字段同时非法时，
	// map 迭代顺序会让报错字段在多次调用间随机变化。
	for _, f := range []struct{ name, val string }{
		{"accent", t.Accent},
		{"accentDark", t.AccentDark},
	} {
		if f.val != "" && !hexColor.MatchString(f.val) {
			return fmt.Errorf("%s 必须是 #rrggbb 格式", f.name)
		}
	}
	if t.Radius != nil && (*t.Radius < 0 || *t.Radius > 24) {
		return fmt.Errorf("radius 必须在 0–24 之间")
	}
	return nil
}

// Normalize 补齐默认值并统一大小写，调用前需通过 Validate。
func (t Theme) Normalize() Theme {
	if t.Mode == "" {
		t.Mode = "auto"
	}
	t.Accent = strings.ToLower(t.Accent)
	t.AccentDark = strings.ToLower(t.AccentDark)
	return t
}

// Parse 解析已保存的主题；空值或字面量 null 返回默认主题。
func Parse(raw string) (Theme, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return Default(), nil
	}
	var t Theme
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return Theme{}, err
	}
	if err := t.Validate(); err != nil {
		return Theme{}, err
	}
	return t.Normalize(), nil
}

// Marshal 返回可持久化的 JSON。
func (t Theme) Marshal() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(t.Normalize())
	return string(b), err
}
