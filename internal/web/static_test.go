package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motao/rollcall/internal/store"
)

func staticReq(h http.Handler, path, accept, inm string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	if inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestStaticAssetsGzipAndETag 静态 JS/CSS 必须支持 gzip 与 304 重校验，
// 否则每次打开面板都要重新传约 300KB 资源。
func TestStaticAssetsGzipAndETag(t *testing.T) {
	h := New(store.New(t.TempDir()), nil, nil, Config{}).Handler()
	raw, err := assets.ReadFile("dist/app.js")
	if err != nil {
		t.Fatal(err)
	}

	rec := staticReq(h, "/app.js", "gzip", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/app.js 应 200，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("应返回 gzip，实际 Content-Encoding=%q", got)
	}
	if v := rec.Header().Values("Vary"); len(v) == 0 || !strings.Contains(strings.Join(v, ","), "Accept-Encoding") {
		t.Fatalf("压缩响应必须带 Vary: Accept-Encoding，实际 %v", v)
	}
	if rec.Body.Len() >= len(raw) {
		t.Errorf("压缩后体积未减小: gzip=%d raw=%d", rec.Body.Len(), len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("响应不是合法 gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("gzip 解压内容与内嵌 app.js 不一致")
	}

	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("静态资源应带弱 ETag，实际 %q", etag)
	}
	rec304 := staticReq(h, "/app.js", "gzip", etag)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match 应返回 304，实际 %d", rec304.Code)
	}
	if rec304.Body.Len() != 0 {
		t.Errorf("304 不应有正文，实际 %d 字节", rec304.Body.Len())
	}

	// 不支持 gzip 的客户端仍拿到原始内容与同一个 ETag
	recIdentity := staticReq(h, "/app.js", "", "")
	if recIdentity.Code != http.StatusOK || recIdentity.Header().Get("Content-Encoding") != "" {
		t.Fatalf("不声明 gzip 时不应压缩: %d %q", recIdentity.Code, recIdentity.Header().Get("Content-Encoding"))
	}
	if !bytes.Equal(recIdentity.Body.Bytes(), raw) {
		t.Fatal("未压缩响应内容不符")
	}
	if recIdentity.Header().Get("ETag") != etag {
		t.Error("压缩与未压缩表示应使用同一个弱校验值")
	}

	// HTML 入口同样压缩
	recIndex := staticReq(h, "/", "gzip", "")
	if recIndex.Code != http.StatusOK || recIndex.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("首页应 gzip: %d %q", recIndex.Code, recIndex.Header().Get("Content-Encoding"))
	}

	// 安装脚本绝不能压缩：未解压的脚本会被直接交给 sh
	recSh := staticReq(h, "/agent/install.sh", "gzip", "")
	if recSh.Code != http.StatusOK {
		t.Fatalf("install.sh 应 200，实际 %d", recSh.Code)
	}
	if ce := recSh.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("install.sh 不应被压缩，实际 Content-Encoding=%q", ce)
	}
	if !strings.HasPrefix(recSh.Body.String(), "#!/bin/sh") {
		t.Fatal("install.sh 正文不是 shell 脚本")
	}
}
