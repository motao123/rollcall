package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// 审计日志的保留策略。管理操作频率很低，90 天足以回溯一次事故；
// 内存只保留最近 1 万条，避免长时间运行后常驻内存随操作数线性增长。
const (
	AuditRetention    = 90 * 24 * time.Hour
	auditMemoryLimit  = 10000
	auditFileLimit    = 50000
	auditPruneBytes   = 8 << 20
	auditPruneEvery   = 2000
	auditQueryMax     = 200
	auditDetailMaxLen = 200
)

// AuditQuery 是审计日志的查询条件。时间范围按 Unix 秒闭区间。
type AuditQuery struct {
	Limit  int
	Offset int
	Actor  string
	Action string
	Search string
	Since  int64
	Until  int64
}

// AddAudit 追加一条审计记录。失败的写盘只记日志，不影响业务请求。
func (s *Store) AddAudit(e model.AuditEntry) {
	if s.auditPath == "" {
		return
	}
	if e.TS <= 0 {
		e.TS = time.Now().Unix()
	}
	if e.ID == "" {
		e.ID = NewID("au")
	}
	if len([]rune(e.Detail)) > auditDetailMaxLen {
		e.Detail = string([]rune(e.Detail)[:auditDetailMaxLen])
	}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	s.audit = append(s.audit, &e)
	if len(s.audit) > auditMemoryLimit {
		drop := len(s.audit) - auditMemoryLimit
		s.audit = append([]*model.AuditEntry(nil), s.audit[drop:]...)
	}
	if err := s.appendAuditLocked(&e); err != nil {
		log.Printf("写入审计日志失败: %v", err)
	}
	s.auditWrites++
	if s.auditWrites%auditPruneEvery == 0 {
		s.pruneAuditLocked(false)
	}
}

func (s *Store) appendAuditLocked(e *model.AuditEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(s.auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// QueryAudit 按时间倒序返回匹配记录与匹配总数。
func (s *Store) QueryAudit(q AuditQuery) ([]model.AuditEntry, int) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > auditQueryMax {
		limit = auditQueryMax
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	actor := strings.ToLower(strings.TrimSpace(q.Actor))
	action := strings.ToLower(strings.TrimSpace(q.Action))
	search := strings.ToLower(strings.TrimSpace(q.Search))

	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	out := make([]model.AuditEntry, 0, limit)
	total, skipped := 0, 0
	for i := len(s.audit) - 1; i >= 0; i-- {
		e := s.audit[i]
		if q.Since > 0 && e.TS < q.Since {
			continue
		}
		if q.Until > 0 && e.TS > q.Until {
			continue
		}
		if actor != "" && !strings.Contains(strings.ToLower(e.Actor), actor) {
			continue
		}
		if action != "" && strings.ToLower(e.Action) != action {
			continue
		}
		if search != "" {
			hay := strings.ToLower(e.Actor + " " + e.Path + " " + e.Action + " " + e.IP + " " + e.Detail)
			if !strings.Contains(hay, search) {
				continue
			}
		}
		total++
		if skipped < offset {
			skipped++
			continue
		}
		if len(out) < limit {
			out = append(out, *e)
		}
	}
	return out, total
}

// AuditStats 返回内存中审计记录的条数与时间跨度。
func (s *Store) AuditStats() map[string]any {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	var earliest, latest int64
	if len(s.audit) > 0 {
		earliest = s.audit[0].TS
		latest = s.audit[len(s.audit)-1].TS
	}
	size := int64(0)
	if fi, err := os.Stat(s.auditPath); err == nil {
		size = fi.Size()
	}
	return map[string]any{
		"count":         len(s.audit),
		"earliest":      earliest,
		"latest":        latest,
		"bytes":         size,
		"retentionDays": int(AuditRetention / (24 * time.Hour)),
	}
}

// readAuditFile 读取审计文件，按保留期过滤并限制条数。
// 第二个返回值表示是否丢弃过内容（需要重写文件）。
func readAuditFile(path string) ([]*model.AuditEntry, bool) {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("读取审计日志失败: %v", err)
		}
		return nil, false
	}
	cutoff := time.Now().Add(-AuditRetention).Unix()
	kept := make([]*model.AuditEntry, 0, 1024)
	dropped := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e model.AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.TS <= 0 {
			dropped = true
			continue
		}
		if e.TS < cutoff {
			dropped = true
			continue
		}
		kept = append(kept, &e)
	}
	if err := sc.Err(); err != nil {
		log.Printf("审计日志读取中断: %v", err)
		dropped = true
	}
	if closeErr := f.Close(); closeErr != nil {
		log.Printf("关闭审计日志失败: %v", closeErr)
	}
	if len(kept) > auditFileLimit {
		kept = kept[len(kept)-auditFileLimit:]
		dropped = true
	}
	return kept, dropped
}

func (s *Store) loadAudit() {
	kept, dropped := readAuditFile(s.auditPath)
	s.auditMu.Lock()
	if len(kept) > auditMemoryLimit {
		s.audit = kept[len(kept)-auditMemoryLimit:]
	} else {
		s.audit = kept
	}
	if dropped {
		s.rewriteAuditLocked(kept)
	}
	s.auditMu.Unlock()
}

// pruneAuditLocked 在文件过大时按保留期重写。
// force=false 时只在超过 auditPruneBytes 才执行，避免每次启动都全量重写。
//
// 重写的数据来自磁盘而不是内存：内存只保留最近 1 万条，
// 用它重写会把文件中更早的 4 万条一并抹掉。
func (s *Store) pruneAuditLocked(force bool) {
	fi, err := os.Stat(s.auditPath)
	if err != nil {
		return
	}
	if !force && fi.Size() < auditPruneBytes {
		return
	}
	kept, dropped := readAuditFile(s.auditPath)
	if !dropped {
		return
	}
	s.rewriteAuditLocked(kept)
}

func (s *Store) rewriteAuditLocked(entries []*model.AuditEntry) {
	if s.auditPath == "" {
		return
	}
	tmp := s.auditPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("重写审计日志失败: %v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			log.Printf("重写审计日志失败: %v", err)
			return
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		log.Printf("重写审计日志失败: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		log.Printf("重写审计日志失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.auditPath); err != nil {
		_ = os.Remove(tmp)
		log.Printf("替换审计日志失败: %v", err)
	}
}

// AuditPath 返回审计文件路径，供健康检查与文档展示。
func (s *Store) AuditPath() string { return s.auditPath }
