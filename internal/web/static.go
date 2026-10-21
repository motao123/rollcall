package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// withStaticETag 为内嵌静态资源加弱 ETag，并处理 If-None-Match。
//
// 静态资源已设置 Cache-Control: no-cache（每次回源校验），
// 没有 ETag 时每次刷新都要传完整 JS/CSS；加上 304 后只剩几百字节。
// 内容在编译期嵌入，哈希只需在启动时算一次。
func withStaticETag(fsys fs.FS, next http.Handler) http.Handler {
	etags := staticETags(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if key == "/" {
			key = "/index.html"
		}
		etag, ok := etags[key]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("ETag", etag)
		// RFC 9110 §13.1.2：If-None-Match 只对 GET/HEAD 有意义。
		// 其它方法（POST/PUT/DELETE）命中静态路径时必须继续走 next，
		// 否则会收到 304 而误以为操作成功。
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if etagMatches(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func staticETags(fsys fs.FS) map[string]string {
	out := make(map[string]string)
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		out["/"+p] = `W/"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	return out
}

// etagMatches 按 RFC 9110 的弱比较解析 If-None-Match 列表。
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}

// gzipPath 只压缩浏览器加载的文本资源与 API JSON/CSV。
// 刻意不压缩 /agent/install.sh：curl 默认不带 Accept-Encoding，
// 但代理或客户端声明 gzip 时，未解压的脚本会被直接交给 sh。
func gzipPath(p string) bool {
	// 事件流必须排除：gzip 会攒够一块才输出，实时推送会被缓冲成"批量下发"；
	// 而且压缩包装会遮住 http.Flusher，SSE 直接不可用。
	if p == "/api/stream" || p == "/api/status/stream" {
		return false
	}
	if strings.HasPrefix(p, "/api/") {
		return true
	}
	if p == "/" || p == "/status" {
		return true
	}
	switch path.Ext(p) {
	case ".js", ".css", ".html":
		return true
	default:
		return false
	}
}

func compressibleContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "text/") // html/css/javascript/csv/plain 由调用方限定路径
}
