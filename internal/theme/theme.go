// Package theme 定义受限的外观主题。
//
// 主题永远不会作为 CSS 文本下发：服务端只接受一组固定字段，
// 客户端也只把它们映射到白名单 CSS 变量，避免主题编辑器变成 XSS 入口。
package theme

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	r := DefaultRadius
	return Theme{Mode: "auto", Radius: &r}
}

// DefaultRadius 是未指定圆角时使用的像素值，与内置 CSS 保持一致。
const DefaultRadius = 10

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
	if t.Radius == nil {
		// 文档写的是「nil = 默认 10」，但之前没有在这里落地，
		// 前端表单、状态页、导出各自复刻这个默认值，已经出现不一致。
		// 统一在归一化阶段落成具体值。
		r := DefaultRadius
		t.Radius = &r
	}
	t.Accent = strings.ToLower(t.Accent)
	t.AccentDark = strings.ToLower(t.AccentDark)
	return t
}

// Parse 解析已保存的主题；空值表示尚未配置，返回默认主题。
// 已保存的 JSON 必须严格合法：未知字段、null、尾随内容都视为配置损坏，
// 否则错误会被静默吞掉，管理员看到的是另一套样式却查不到原因。
func Parse(raw string) (Theme, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Default(), nil
	}
	var tp *Theme
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tp); err != nil {
		return Theme{}, err
	}
	if tp == nil {
		return Theme{}, errors.New("主题配置不能为 null")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Theme{}, errors.New("主题配置包含多余内容")
	}
	if err := tp.Validate(); err != nil {
		return Theme{}, err
	}
	return tp.Normalize(), nil
}

// Marshal 返回可持久化的 JSON。
func (t Theme) Marshal() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(t.Normalize())
	return string(b), err
}
