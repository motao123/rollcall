package selfupdate

import (
	"context"
	"strings"
	"testing"
)

// TestFetchRevalidatesURLAndZeroValue 是回归测试。
//
// Fetch 是导出方法，Updater 的字段也可写：原实现直接用 u.ManifestURL
// 和 u.client，于是
//   - 把 URL 改成 http:// 就绕过了 New 里的 HTTPS 校验（清单与签名可被中间人替换）；
//   - 零值 Updater（或任何非 New 构造）在 client 为 nil 时直接空指针崩溃。
func TestFetchRevalidatesURLAndZeroValue(t *testing.T) {
	u := &Updater{ManifestURL: "http://example.com/manifest.json"}
	if _, err := u.Fetch(context.Background()); err == nil {
		t.Fatal("明文 HTTP 清单地址应被拒绝")
	} else if !strings.Contains(err.Error(), "地址") {
		t.Fatalf("错误信息应指出地址问题: %v", err)
	}

	// 零值：不能 panic，应返回错误
	var zero Updater
	if _, err := zero.Fetch(context.Background()); err == nil {
		t.Fatal("空清单地址应报错")
	}

	// Download 也不能因 client 为 nil 而崩溃
	if _, err := zero.Download(context.Background(), Asset{URL: "https://example.com/a"}, t.TempDir()); err == nil {
		t.Fatal("空资产应报错")
	}
}
