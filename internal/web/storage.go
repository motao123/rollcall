package web

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

type storageEntry struct {
	Name  string `json:"name"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

type nodeHistoryStat struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Points map[string]int `json:"points"`
}

// handleAdminStorage 汇总数据目录占用与历史容量。
//
//	GET /api/admin/storage
//
// 只统计大小与点数，不读取内容：状态页之外的管理页偶尔打开一次，
// 走一遍目录元数据即可；不需要为容量展示引入常驻扫描。
func (s *Server) handleAdminStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var total int64
	entries := make([]storageEntry, 0, 8)
	if s.dataDir != "" {
		if items, err := os.ReadDir(s.dataDir); err == nil {
			for _, it := range items {
				if strings.HasPrefix(it.Name(), ".") {
					continue
				}
				ent := storageEntry{Name: it.Name()}
				p := filepath.Join(s.dataDir, it.Name())
				_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return nil
					}
					if info, err := d.Info(); err == nil {
						ent.Files++
						ent.Bytes += info.Size()
					}
					return nil
				})
				total += ent.Bytes
				entries = append(entries, ent)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Bytes != entries[j].Bytes {
			return entries[i].Bytes > entries[j].Bytes
		}
		return entries[i].Name < entries[j].Name
	})

	nodes := s.st.Nodes()
	hist := make([]nodeHistoryStat, 0, len(nodes))
	totals := map[string]int{
		store.HistoryResolution1m: 0,
		store.HistoryResolution5m: 0,
		store.HistoryResolution1h: 0,
	}
	for _, n := range nodes {
		counts := s.st.HistoryCounts(n.ID)
		for k, v := range counts {
			totals[k] += v
		}
		hist = append(hist, nodeHistoryStat{ID: n.ID, Name: n.Name, Points: counts})
	}
	sort.Slice(hist, func(i, j int) bool {
		return hist[i].Points[store.HistoryResolution1h] > hist[j].Points[store.HistoryResolution1h]
	})

	ret := store.CurrentHistoryRetention()
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }

	resp := map[string]any{
		"dataDir":    s.dataDir,
		"totalBytes": total,
		"entries":    entries,
		"history": map[string]any{
			"nodes":  hist,
			"totals": totals,
			"retentionDays": map[string]int{
				"1m": days(ret.Tier1m),
				"5m": days(ret.Tier5m),
				"1h": days(ret.Tier1h),
			},
		},
		"audit": s.st.AuditStats(),
	}
	if s.autoBackup != nil {
		bs := s.autoBackup.Status()
		resp["backup"] = map[string]any{
			"enabled":    true,
			"files":      len(bs.Files),
			"totalBytes": bs.TotalBytes,
			"lastAt":     bs.LastAt,
		}
	} else {
		resp["backup"] = map[string]any{"enabled": false}
	}
	writeJSON(w, http.StatusOK, resp)
}
