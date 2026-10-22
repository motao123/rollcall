// Package backupjob 定期导出加密配置备份并执行保留策略。
//
// 与手动备份共用 ExportConfig + backup.Encrypt，但多了一步落盘后解密校验：
// 备份文件"看起来生成了"不代表将来能恢复，磁盘写入截断、加密参数错误
// 都只在真正恢复时才暴露。
package backupjob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/motao123/rollcall/internal/backup"
	"github.com/motao123/rollcall/internal/store"
)

const (
	MinInterval    = time.Minute
	failureRetry   = time.Hour
	notifyCooldown = 6 * time.Hour
	statusFileName = "backup-status.json"
	filePrefix     = "rollcall-auto-"
)

type Config struct {
	Dir      string
	Interval time.Duration
	Keep     int
	Password string
	Store    *store.Store
	// Notify 在连续失败时调用；可为空。
	Notify func(error)
}

type File struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

type Status struct {
	Enabled         bool   `json:"enabled"`
	Dir             string `json:"dir,omitempty"`
	IntervalSeconds int64  `json:"intervalSeconds"`
	Keep            int    `json:"keep"`
	LastAt          int64  `json:"lastAt"`
	LastError       string `json:"lastError,omitempty"`
	LastFile        string `json:"lastFile,omitempty"`
	LastSize        int64  `json:"lastSize"`
	LastChecksum    string `json:"lastChecksum,omitempty"`
	NextAt          int64  `json:"nextAt,omitempty"`
	Runs            int64  `json:"runs"`
	Failures        int64  `json:"failures"`
	Files           []File `json:"files"`
	TotalBytes      int64  `json:"totalBytes"`
}

type Result struct {
	File     string `json:"file"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	Duration int64  `json:"durationMs"`
}

type persisted struct {
	LastAt       int64  `json:"lastAt"`
	LastError    string `json:"lastError,omitempty"`
	LastFile     string `json:"lastFile,omitempty"`
	LastSize     int64  `json:"lastSize"`
	LastChecksum string `json:"lastChecksum,omitempty"`
	Runs         int64  `json:"runs"`
	Failures     int64  `json:"failures"`
}

type Manager struct {
	cfg        Config
	statusPath string

	runMu sync.Mutex
	mu    sync.Mutex
	st    persisted
	next  time.Time

	lastNotify time.Time
	stop       chan struct{}
	done       chan struct{}
	startOnce  sync.Once
	stopOnce   sync.Once
}

func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("backupjob: store 不能为空")
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("backupjob: 备份目录不能为空")
	}
	if len(cfg.Password) < 8 {
		return nil, errors.New("backupjob: 备份密码至少 8 个字符")
	}
	if cfg.Interval < MinInterval {
		cfg.Interval = MinInterval
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 14
	}
	m := &Manager{cfg: cfg, statusPath: filepath.Join(cfg.Dir, statusFileName)}
	m.loadStatus()
	m.next = time.Now().Add(30 * time.Second)
	if m.st.LastAt > 0 {
		m.next = time.Unix(m.st.LastAt, 0).Add(cfg.Interval)
	}
	return m, nil
}

func (m *Manager) Start() {
	m.startOnce.Do(func() {
		m.stop = make(chan struct{})
		m.done = make(chan struct{})
		go m.loop()
	})
}

func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		if m.stop == nil {
			return
		}
		close(m.stop)
		select {
		case <-m.done:
		case <-time.After(20 * time.Second):
			log.Printf("自动备份任务 20 秒内未退出")
		}
	})
}

func (m *Manager) loop() {
	defer close(m.done)
	for {
		m.mu.Lock()
		next := m.next
		m.mu.Unlock()
		wait := time.Until(next)
		if wait < time.Second {
			wait = time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			// ★ 定时器一旦排定就固定了 deadline，而 RunNow（手动触发或
			// 上一次定时执行）会把 m.next 往后推。到点后必须重新确认：
			// 若 next 已被推后，说明这次运行已经有人做过了 —— 直接再跑
			// 会立刻多出一份重复备份，在 keep=14 时挤掉一份历史。
			// 实测路径：启动时首次运行排在 30 秒后，期间手动点「立即备份」
			// 仍会在 30 秒到点补跑一次。
			m.mu.Lock()
			due := m.next
			m.mu.Unlock()
			if time.Now().Before(due) {
				continue
			}
			if _, err := m.RunNow(); err != nil {
				log.Printf("自动备份失败: %v", err)
			}
		case <-m.stop:
			t.Stop()
			return
		}
	}
}

// RunNow 立即执行一次备份。并发调用会串行等待。
func (m *Manager) RunNow() (Result, error) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	started := time.Now()
	res, err := m.run()
	if err != nil {
		m.recordFailure(err, started)
		return Result{}, err
	}
	m.recordSuccess(res, started)
	return res, nil
}

func (m *Manager) run() (Result, error) {
	start := time.Now()
	if err := os.MkdirAll(m.cfg.Dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("创建备份目录: %w", err)
	}
	raw, err := m.cfg.Store.ExportConfig()
	if err != nil {
		return Result{}, fmt.Errorf("导出配置: %w", err)
	}
	enc, err := backup.Encrypt(raw, m.cfg.Password)
	if err != nil {
		return Result{}, fmt.Errorf("加密备份: %w", err)
	}
	// 落盘前先自检：解密结果必须与原文逐字节一致，且仍是合法 JSON。
	plain, err := backup.Decrypt(enc, m.cfg.Password)
	if err != nil {
		return Result{}, fmt.Errorf("备份自检失败: %w", err)
	}
	if !bytes.Equal(plain, raw) {
		return Result{}, errors.New("备份自检失败：解密内容与原文不一致")
	}
	if !json.Valid(plain) {
		return Result{}, errors.New("备份自检失败：解密内容不是合法 JSON")
	}
	name := m.uniqueName(start)
	path := filepath.Join(m.cfg.Dir, name)
	tmp, err := os.CreateTemp(m.cfg.Dir, "."+filePrefix+"*.tmp")
	if err != nil {
		return Result{}, fmt.Errorf("创建临时备份文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("设置备份文件权限: %w", err)
	}
	if _, err := tmp.Write(enc); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("写入备份: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("同步备份: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Result{}, fmt.Errorf("关闭备份文件: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return Result{}, fmt.Errorf("保存备份: %w", err)
	}
	if err := m.prune(); err != nil {
		// 备份本身已成功，保留策略失败只记日志，不把本次运行标成失败。
		log.Printf("自动备份保留策略执行失败: %v", err)
	}
	return Result{
		File:     name,
		Size:     int64(len(enc)),
		Checksum: backup.Checksum(enc),
		Duration: time.Since(start).Milliseconds(),
	}, nil
}

func (m *Manager) uniqueName(t time.Time) string {
	base := filePrefix + t.Format("20060102-150405")
	name := base + ".rcbk"
	for i := 1; i < 1000; i++ {
		if _, err := os.Stat(filepath.Join(m.cfg.Dir, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d.rcbk", base, i)
	}
	return fmt.Sprintf("%s-%d.rcbk", base, t.UnixNano())
}

func (m *Manager) prune() error {
	entries, err := m.listFiles()
	if err != nil {
		return err
	}
	for i := m.cfg.Keep; i < len(entries); i++ {
		if err := os.Remove(filepath.Join(m.cfg.Dir, entries[i].Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (m *Manager) listFiles() ([]File, error) {
	ents, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]File, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), ".rcbk") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime == out[j].ModTime {
			return out[i].Name > out[j].Name
		}
		return out[i].ModTime > out[j].ModTime
	})
	return out, nil
}

func (m *Manager) recordSuccess(res Result, at time.Time) {
	m.mu.Lock()
	m.st.LastAt = at.Unix()
	m.st.LastError = ""
	m.st.LastFile = res.File
	m.st.LastSize = res.Size
	m.st.LastChecksum = res.Checksum
	m.st.Runs++
	m.next = at.Add(m.cfg.Interval)
	m.mu.Unlock()
	m.saveStatus()
}

func (m *Manager) recordFailure(err error, at time.Time) {
	m.mu.Lock()
	m.st.LastAt = at.Unix()
	m.st.LastError = err.Error()
	m.st.Runs++
	m.st.Failures++
	retry := m.cfg.Interval
	if retry > failureRetry {
		retry = failureRetry
	}
	m.next = at.Add(retry)
	notify := m.cfg.Notify != nil && (m.lastNotify.IsZero() || at.Sub(m.lastNotify) >= notifyCooldown)
	if notify {
		m.lastNotify = at
	}
	m.mu.Unlock()
	m.saveStatus()
	if notify {
		m.cfg.Notify(err)
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	st := m.st
	next := m.next
	m.mu.Unlock()
	files, err := m.listFiles()
	if err != nil {
		log.Printf("列出自动备份文件失败: %v", err)
	}
	if files == nil {
		files = []File{}
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return Status{
		Enabled:         true,
		Dir:             m.cfg.Dir,
		IntervalSeconds: int64(m.cfg.Interval / time.Second),
		Keep:            m.cfg.Keep,
		LastAt:          st.LastAt,
		LastError:       st.LastError,
		LastFile:        st.LastFile,
		LastSize:        st.LastSize,
		LastChecksum:    st.LastChecksum,
		NextAt:          next.Unix(),
		Runs:            st.Runs,
		Failures:        st.Failures,
		Files:           files,
		TotalBytes:      total,
	}
}

func (m *Manager) loadStatus() {
	b, err := os.ReadFile(m.statusPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("读取自动备份状态失败: %v", err)
		return
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		log.Printf("自动备份状态文件损坏，已忽略: %v", err)
		return
	}
	m.st = p
}

func (m *Manager) saveStatus() {
	m.mu.Lock()
	p := m.st
	m.mu.Unlock()
	b, err := json.Marshal(p)
	if err != nil {
		log.Printf("编码自动备份状态失败: %v", err)
		return
	}
	tmp := m.statusPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		log.Printf("写入自动备份状态失败: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statusPath); err != nil {
		_ = os.Remove(tmp)
		log.Printf("替换自动备份状态失败: %v", err)
	}
}
