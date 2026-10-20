package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/backup"
	"github.com/motao123/rollcall/internal/notify"
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
		fail(w, http.StatusBadRequest, "请确认导入会覆盖备份中包含的配置项")
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
	// ★ 先校验备份里的通知配置，再落库。
	// 反过来做（先导入、失败只回 warning）会把一份非法的 notify 配置
	// 持久化下来：运行态仍是旧配置，重启时 notify.Parse 失败，通知直接静默停摆。
	var peek struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(raw, &peek); err == nil {
		if nv := strings.TrimSpace(peek.Settings["notify"]); nv != "" {
			nc, perr := notify.Parse(nv)
			if perr != nil {
				fail(w, http.StatusBadRequest, "备份中的通知配置无法解析，已拒绝导入: "+perr.Error())
				return
			}
			if verr := nc.Validate(); verr != nil {
				fail(w, http.StatusBadRequest, "备份中的通知配置不合法，已拒绝导入: "+verr.Error())
				return
			}
		}
	}
	if err := s.st.ImportConfig(raw); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 设置里包含通知配置；导入后必须**先校验**再重新装载。
	// 校验失败不能静默：备份已经落库，运行态却还是旧配置，
	// 管理员会看到"导入成功"但通知行为没变（直到重启才在日志里发现）。
	warning := ""
	s.notifySaveMu.Lock()
	defer s.notifySaveMu.Unlock()
	if nc, err := notify.Parse(s.st.GetSetting("notify")); err != nil {
		log.Printf("导入后通知配置解析失败: %v", err)
		warning = "配置已导入，但通知配置无法解析，未生效: " + err.Error()
	} else if verr := nc.Validate(); verr != nil {
		log.Printf("导入后通知配置校验失败: %v", verr)
		warning = "配置已导入，但通知配置不合法，未生效: " + verr.Error()
	} else {
		s.applyNotifyConfig(nc)
	}

	resp := map[string]any{
		"ok":       true,
		"nodes":    len(s.st.Nodes()),
		"services": len(s.st.Services()),
		"rules":    len(s.st.Rules()),
	}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}
