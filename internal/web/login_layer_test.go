package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestPanelHiddenElementsKeepHiddenStyle 是前端资源级回归。
//
// 本项目已经两次踩中同一个坑：给带 hidden 属性的元素写 `#id { display:flex }`，
// 该声明会覆盖 UA 样式表的 `[hidden]{display:none}` —— 元素"藏不起来"：
//
//	第一次是节点批量操作条在未选中时也常显（.batch-bar）；
//	第二次是登录层把仪表盘整屏盖成白屏（#loginLayer，且 DOM 正确、
//	只有截图/肉眼能发现，功能测试全部通过）。
//
// 因此把规则钉死：index.html 里带 hidden 属性的元素，如果 CSS 对它声明了
// display，就必须同时存在 `#id[hidden]` 规则。新增带 hidden 的元素时，
// 这个测试会强制作者想起这个陷阱。
func TestPanelHiddenElementsKeepHiddenStyle(t *testing.T) {
	html, err := os.ReadFile("dist/index.html")
	if err != nil {
		t.Fatalf("读取 index.html 失败: %v", err)
	}
	css, err := os.ReadFile("dist/style.css")
	if err != nil {
		t.Fatalf("读取 style.css 失败: %v", err)
	}
	cssText := string(css)

	// 提取带 hidden 属性的元素 id（<xx ... id="x" ... hidden ...>）
	// 注意：Go 的 RE2 不支持 (?!) 前瞻，这里用 \bhidden\b 即可——
	// 属性名 hidden 与 hidden="" 都能匹配，而 data-hidden 会被 \b 挡住一半，
	// 即使误匹配也只是多查一个 id，不影响判定正确性。
	idRe := regexp.MustCompile(`<([a-z]+)[^>]*\bid="([A-Za-z][\w-]*)"[^>]*>`)
	hiddenRe := regexp.MustCompile(`\bhidden\b`)

	checked := 0
	for _, m := range idRe.FindAllStringSubmatch(string(html), -1) {
		tag, id := m[1], m[2]
		if !hiddenRe.MatchString(m[0]) {
			continue
		}
		checked++

		// 该 id 是否存在设置 display 的规则（粗匹配块级声明即可：
		// 目的是发现"display 覆盖 hidden"这一模式，而非解析完整 CSS）。
		displayRule := regexp.MustCompile(`(?s)#` + regexp.QuoteMeta(id) + `\s*\{[^}]*display\s*:`)
		if !displayRule.MatchString(cssText) {
			continue // CSS 没动 display，UA 的 [hidden] 生效，安全
		}
		if !strings.Contains(cssText, "#"+id+"[hidden]") {
			t.Errorf("index.html 中 #%s(%s) 带 hidden 属性，但 style.css 为它声明了 display 却没有配套的 #%s[hidden] 规则；"+
				"display 会覆盖 [hidden]{display:none}，元素将藏不住（本项目已因此出过两次线上问题）", id, tag, id)
		}
	}
	if checked == 0 {
		t.Fatal("未在 index.html 中找到任何带 hidden 的元素，测试前提失效——请确认标记未重写")
	}
	if !strings.Contains(cssText, "#loginLayer") {
		t.Fatal("style.css 缺少登录层样式，登录页回归为骨架内嵌小卡")
	}
	if !strings.Contains(cssText, "body.auth-gate .app") {
		t.Fatal("style.css 缺少门禁期隐藏后台骨架的规则，登录页会与仪表盘同屏")
	}
}
