package web

import (
	"io/fs"
	"net/http"
)

// handleMonitorPage 输出监控面板的 HTML 外壳（dist/monitor.html）。
//
// 为什么不用 FileServer 直接处理 /monitor：
// 静态文件在 dist 下叫 monitor.html，对外路径是 /monitor（无扩展名、无尾斜杠），
// http.FileServer 会按目录处理并返回 404。这里读一次嵌入文件直接写出。
//
// 页面本身不含任何数据：指标由 /api/status 提供，因此与公开页共用同一道
// publicGate 门槛 —— 公开页未开启时 /monitor 一并 404，不会出现
// "页面能打开但所有图表都空"的半残状态。
func (s *Server) handleMonitorPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	b, err := fs.ReadFile(assets, "dist/monitor.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, "监控面板资源缺失")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}
