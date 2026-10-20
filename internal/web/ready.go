package web

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/version"
)

// readyProbeTTL 限制数据目录可写探测的频率：/readyz 是公开端点，
// 每次请求都落盘一个小文件既浪费 I/O，也给匿名请求放大了写放大。
const readyProbeTTL = 15 * time.Second

// readyProbe 缓存最近一次可写探测结果。
type readyProbe struct {
	mu  sync.Mutex
	at  time.Time
	err error
}

// handleReadyz 是面向升级脚本与编排系统的深度健康检查。
//
// /healthz 只证明进程还在响应；这里的 200 还要求存储已载入、
// 数据目录可写、快照没有持续保存失败。升级脚本用它决定是否回滚。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if reason := s.readiness(); reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready":   false,
			"reason":  reason,
			"version": version.Version,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":   true,
		"version": version.Version,
	})
}

// readiness 返回未就绪原因（空字符串表示就绪）。
// 对外只暴露固定的分类，具体错误写日志，避免匿名访客拿到路径等内部信息。
func (s *Server) readiness() string {
	if s.st == nil {
		return "store not initialized"
	}
	if err := s.st.LoadErr(); err != nil {
		log.Printf("readyz: 状态载入失败: %v", err)
		return "state load failed"
	}
	if dir := s.readyDataDir(); dir != "" {
		if err := s.probeDataDir(dir); err != nil {
			log.Printf("readyz: 数据目录不可写: %v", err)
			return "data dir not writable"
		}
	}

	stats := s.st.SaveStats()
	if stats.LastSuccess == 0 {
		// 启动初期还没到首个保存周期，不算故障；连续多次尝试全部失败
		// 才说明磁盘满/权限错误等持久问题。
		if stats.Count >= 3 && stats.Failures >= stats.Count {
			log.Printf("readyz: 快照连续保存失败 %d 次", stats.Failures)
			return "snapshot save failing"
		}
		if s.saveInterval > 0 && time.Since(s.startedAt) > 2*s.saveInterval {
			log.Printf("readyz: 启动 %s 后仍未成功保存过快照", time.Since(s.startedAt).Round(time.Second))
			return "no snapshot saved"
		}
		return ""
	}

	if s.saveInterval > 0 {
		stale := 3 * s.saveInterval
		if stale < 5*time.Minute {
			// 小间隔部署也要容忍磁盘短暂抖动，避免升级时因一次慢写回滚。
			stale = 5 * time.Minute
		}
		if age := time.Since(time.Unix(stats.LastSuccess, 0)); age > stale {
			log.Printf("readyz: 快照已 %s 未成功保存", age.Round(time.Second))
			return "snapshot stale"
		}
	}
	return ""
}

func (s *Server) readyDataDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	if s.userPath != "" {
		return filepath.Dir(s.userPath)
	}
	return ""
}

func (s *Server) probeDataDir(dir string) error {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if !s.ready.at.IsZero() && time.Since(s.ready.at) < readyProbeTTL {
		return s.ready.err
	}
	err := writeProbeFile(dir)
	s.ready.at = time.Now()
	s.ready.err = err
	return err
}

func writeProbeFile(dir string) error {
	f, err := os.CreateTemp(dir, ".readyz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write([]byte("ok"))
	cerr := f.Close()
	rerr := os.Remove(name)
	switch {
	case werr != nil:
		return werr
	case cerr != nil:
		return cerr
	default:
		return rerr
	}
}
