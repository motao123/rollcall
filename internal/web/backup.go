package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/backup"
	"github.com/motao/rollcall/internal/notify"
)

type backupRequest struct {
	Password string `json:"password"`
}

type restoreRequest struct {
	Password string `json:"password"`
	Data     string `json:"data"`
	Confirm  bool   `json:"confirm"`
}

// handleBackup 导出加密的配置备份。
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in backupRequest
	if !decodeAccountJSON(w, r, &in) {
		return
	}
	if len(in.Password) < 8 {
		fail(w, http.StatusBadRequest, "备份密码至少 8 个字符")
		return
	}
	raw, err := s.st.ExportConfig()
	if err != nil {
		fail(w, http.StatusInternalServerError, "导出配置失败: "+err.Error())
		return
	}
	enc, err := backup.Encrypt(raw, in.Password)
	if err != nil {
		fail(w, http.StatusInternalServerError, "加密备份失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filename": "rollcall-backup-" + time.Now().Format("20060102-150405") + ".rcbk",
		"data":     base64.StdEncoding.EncodeToString(enc),
		"size":     len(enc),
		"checksum": backup.Checksum(enc),
	})
}

// handleRestore 导入加密配置备份。监控历史与告警记录不受影响。
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 32<<20)
	var in restoreRequest
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求包含多余内容")
		return
	}
	if !in.Confirm {
		fail(w, http.StatusBadRequest, "请确认导入会替换当前全部配置")
		return
	}
	if in.Password == "" {
		fail(w, http.StatusBadRequest, "请输入备份密码")
		return
	}
	enc, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Data))
	if err != nil || len(enc) == 0 {
		fail(w, http.StatusBadRequest, "备份数据不是有效的 base64")
		return
	}
	raw, err := backup.Decrypt(enc, in.Password)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.ImportConfig(raw); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 设置里包含通知配置；导入后立即重新装载，无需重启。
	if nc, err := notify.Parse(s.st.GetSetting("notify")); err == nil {
		s.applyNotifyConfig(nc)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"nodes":    len(s.st.Nodes()),
		"services": len(s.st.Services()),
		"rules":    len(s.st.Rules()),
	})
}
