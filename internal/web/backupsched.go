package web

import "net/http"

// handleAutoBackup 返回自动备份状态。
//
//	GET /api/admin/backup/auto
func (s *Server) handleAutoBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoBackup == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "未启用：启动时需提供备份密码（-backup-password-file 或 ROLLCALL_BACKUP_PASSWORD）",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.autoBackup.Status())
}

// handleAutoBackupRun 立即执行一次自动备份。
//
//	POST /api/admin/backup/auto/run
func (s *Server) handleAutoBackupRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoBackup == nil {
		fail(w, http.StatusConflict, "自动备份未启用")
		return
	}
	res, err := s.autoBackup.RunNow()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res, "status": s.autoBackup.Status()})
}
