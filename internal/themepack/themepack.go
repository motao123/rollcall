// Package themepack 发现并校验安装在 <数据目录>/themes/<name>/ 下的
// 公开状态页主题包。
//
// 主题包只能提供两个固定文件名：status.css 与 status.js（至少一个），
// 清单 theme.json 只描述元数据。status.js 与站点同源运行，因此
// 安装主题等价于授予管理员权限——这一点必须写在部署文档里。
package themepack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// ManifestName 是主题包清单文件名。
	ManifestName = "theme.json"
	// CSSName 与 JSName 是主题包允许提供的全部资源文件。
	CSSName = "status.css"
	JSName  = "status.js"
	// MaxManifest 是清单大小上限，防止把巨大文件当成配置逐次解析。
	MaxManifest = 4 << 10
	// MaxAsset 是单个样式/脚本的大小上限。
	MaxAsset = 1 << 20
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Manifest 是 theme.json 的受限结构，未知字段一律拒绝。
type Manifest struct {
	Name        string `json:"name"`        // 可选；出现时必须与目录名一致
	Title       string `json:"title"`       // 可选；缺省时用目录名
	Description string `json:"description"` // 可选
	Version     string `json:"version"`     // 可选
	Author      string `json:"author"`      // 可选
}

// Pack 是一个通过校验的主题包。
type Pack struct {
	Name        string
	Title       string
	Description string
	Version     string
	Author      string
	Dir         string
	HasCSS      bool
	HasJS       bool
}

// Entry 是 Discover 的扫描结果；Err 非空表示该目录不能作为主题使用。
type Entry struct {
	Name string
	Pack Pack
	Err  error
}

// ValidName 判断目录名能否作为主题名（同时用于 URL 与服务端路径）。
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Root 返回数据目录下的主题根目录。
func Root(dataDir string) string { return filepath.Join(dataDir, "themes") }

// Discover 扫描 root 下的主题目录，跳过普通文件。
// root 不存在时返回空列表（尚未安装任何主题是正常状态）。
func Discover(root string) []Entry {
	des, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		if !ValidName(name) {
			entries = append(entries, Entry{Name: name,
				Err: errors.New("目录名只能包含小写字母、数字与连字符（1–32 位）")})
			continue
		}
		p, err := Load(root, name)
		if err != nil {
			entries = append(entries, Entry{Name: name, Err: err})
			continue
		}
		entries = append(entries, Entry{Name: name, Pack: p})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// Load 校验并加载单个主题包。
//
// 只接受普通文件：符号链接可能把 themes 目录之外的文件暴露到公开路径，
// 即使写入 themes 本身就需要管理员权限，这里仍按最小权限拒绝。
func Load(root, name string) (Pack, error) {
	if !ValidName(name) {
		return Pack{}, errors.New("目录名只能包含小写字母、数字与连字符（1–32 位）")
	}
	dir := filepath.Join(root, name)
	fi, err := os.Lstat(dir)
	if err != nil {
		return Pack{}, errors.New("主题目录不存在")
	}
	if !fi.IsDir() {
		return Pack{}, errors.New("主题路径不是目录")
	}

	mpath := filepath.Join(dir, ManifestName)
	mfi, err := os.Lstat(mpath)
	if err != nil {
		return Pack{}, fmt.Errorf("缺少 %s", ManifestName)
	}
	if !mfi.Mode().IsRegular() {
		return Pack{}, fmt.Errorf("%s 必须是普通文件（目录或符号链接不接受）", ManifestName)
	}
	if mfi.Size() > MaxManifest {
		return Pack{}, fmt.Errorf("%s 不能超过 %d 字节", ManifestName, MaxManifest)
	}
	b, err := os.ReadFile(mpath)
	if err != nil {
		return Pack{}, fmt.Errorf("读取 %s 失败: %v", ManifestName, err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Pack{}, fmt.Errorf("%s 解析失败: %v", ManifestName, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Pack{}, fmt.Errorf("%s 只能包含单个 JSON 对象", ManifestName)
	}

	m.Name = strings.TrimSpace(m.Name)
	m.Title = strings.TrimSpace(m.Title)
	m.Description = strings.TrimSpace(m.Description)
	m.Version = strings.TrimSpace(m.Version)
	m.Author = strings.TrimSpace(m.Author)
	if m.Name != "" && m.Name != name {
		return Pack{}, fmt.Errorf("%s 的 name 必须与目录名 %q 一致", ManifestName, name)
	}
	for _, f := range []struct {
		label string
		val   string
		max   int
	}{
		{"title", m.Title, 80},
		{"description", m.Description, 500},
		{"version", m.Version, 40},
		{"author", m.Author, 80},
	} {
		if utf8.RuneCountInString(f.val) > f.max {
			return Pack{}, fmt.Errorf("%s 不能超过 %d 个字符", f.label, f.max)
		}
	}

	p := Pack{Name: name, Title: m.Title, Description: m.Description,
		Version: m.Version, Author: m.Author, Dir: dir}
	if p.Title == "" {
		p.Title = name
	}
	for _, f := range []struct {
		name string
		dst  *bool
	}{
		{CSSName, &p.HasCSS},
		{JSName, &p.HasJS},
	} {
		path := filepath.Join(dir, f.name)
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Pack{}, fmt.Errorf("读取 %s 失败: %v", f.name, err)
		}
		if !fi.Mode().IsRegular() {
			return Pack{}, fmt.Errorf("%s 必须是普通文件（目录或符号链接不接受）", f.name)
		}
		if fi.Size() > MaxAsset {
			return Pack{}, fmt.Errorf("%s 不能超过 %d 字节", f.name, MaxAsset)
		}
		*f.dst = true
	}
	if !p.HasCSS && !p.HasJS {
		return Pack{}, fmt.Errorf("至少需要 %s 或 %s", CSSName, JSName)
	}
	return p, nil
}
