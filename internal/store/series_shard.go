package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
)

// 历史分片：每个节点一个文件，放在 <data>/series/<id>.json.gz。
//
// 为什么要把 series 从主快照里拆出去：
//   - 主快照 90%+ 的体积是 series（40 台 24 小时 = 11 万个点，约 30MB JSON）。
//     一次 60 秒快照要先把这 30MB 深拷贝、再编码、再 gzip，实测 662ms（VPS）。
//     拆开后主快照只剩配置与当前态（约 1MB），拷贝与编码都降到毫秒级。
//   - 拆开后可以**增量**写：只有上次保存之后有新点的节点才重写自己的分片。
//     30 秒上报 + 60 秒保存的组合下，每次通常只有一半节点变脏。
//   - 单个分片损坏只影响那台机器的历史图，不再让整份快照被判为损坏。
//
// 代价：快照不再是一个文件。回滚到 v1.11 及更早版本会读不到历史
// （它们只认 state.json 里的 series），这一点写进了 release notes。

const seriesShardVersion = 1

// seriesShard 是单个节点的历史分片内容。
// 带 NodeID 而不是靠文件名反查：文件名可能是十六进制编码（见 shardFileName）。
type seriesShard struct {
	SchemaVersion int             `json:"schemaVersion"`
	NodeID        string          `json:"nodeId"`
	SavedAt       int64           `json:"savedAt"`
	Points        []model.Metrics `json:"points"`
}

func (s *Store) seriesDirPath() string {
	return filepath.Join(filepath.Dir(s.path), "series")
}

// shardFileName 生成分片文件名。
//
// 节点 ID 由 NewID 生成（字母数字 + 下划线），但导入备份进来的 ID 可能含
// 其它字符。对不安全字符整体做十六进制编码，避免路径穿越与非法文件名。
func shardFileName(id string) string {
	if id == "" {
		return "empty.json.gz"
	}
	safe := true
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return id + ".json.gz"
	}
	return "x" + hex.EncodeToString([]byte(id)) + ".json.gz"
}

func (s *Store) shardPath(id string) string {
	return filepath.Join(s.seriesDirPath(), shardFileName(id))
}

// writeSeriesShard 原子写入一个节点的历史分片。
func writeSeriesShard(dir, id string, pts []model.Metrics, savedAt int64) (int64, error) {
	sh := seriesShard{
		SchemaVersion: seriesShardVersion,
		NodeID:        id,
		SavedAt:       savedAt,
		Points:        pts,
	}
	raw, err := json.Marshal(&sh)
	if err != nil {
		return 0, err
	}
	enc, err := encodeSnapshot(raw)
	if err != nil {
		return 0, fmt.Errorf("压缩历史分片 %s: %w", id, err)
	}
	name := shardFileName(id)
	// 临时文件名带上目标名，避免同一目录下多个分片并发写时互相覆盖。
	tmp := filepath.Join(dir, ".tmp-"+name)
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return int64(len(enc)), nil
}

// loadSeriesShards 从目录加载历史分片。
//
// 只在"主快照里没有该节点的 series"时采用：v1.11 及更早的 state.json 内嵌
// series，升级后第一启动会同时存在两处数据。主快照是原子写入的，它要么是
// 完整的旧格式（含 series），要么是新格式（不含），因此**主快照优先**是安全的。
func (s *Store) loadSeriesShards() error {
	dir := s.seriesDirPath()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取历史分片目录 %s: %w", dir, err)
	}

	loaded, skipped := 0, 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json.gz") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			skipped++
			continue
		}
		raw, derr := decodeSnapshot(b)
		if derr != nil {
			skipped++
			continue
		}
		var sh seriesShard
		if jerr := json.Unmarshal(raw, &sh); jerr != nil || sh.NodeID == "" {
			skipped++
			continue
		}
		// 孤儿分片（节点已删除、或备份导入后不存在）直接忽略；
		// 真正删除文件交给 pruneSeriesShards，避免加载路径产生副作用。
		if _, ok := s.nodes[sh.NodeID]; !ok {
			skipped++
			continue
		}

		s.mu.Lock()
		if _, exists := s.series[sh.NodeID]; exists {
			// 主快照里已有（旧格式迁移场景）：保留主快照的数据。
			s.mu.Unlock()
			continue
		}
		sort.Slice(sh.Points, func(i, j int) bool { return sh.Points[i].TS < sh.Points[j].TS })
		r := NewRing(RingSize)
		for _, p := range sh.Points {
			r.Add(p)
		}
		s.series[sh.NodeID] = r
		// 已落盘的内容不需要再写一次：把 seq 与 saved 对齐。
		s.seriesSeq[sh.NodeID] = 1
		s.seriesSaved[sh.NodeID] = 1
		s.mu.Unlock()
		loaded++
	}
	if skipped > 0 {
		// 单个分片损坏不应阻塞启动，但必须留痕：否则历史图"莫名少了一段"。
		log.Printf("历史分片：加载 %d 个，跳过 %d 个（损坏或对应节点不存在）", loaded, skipped)
	}
	return nil
}

// pruneSeriesShards 删除不再对应任何节点的分片文件。
//
// 调用点在 Save 内（已持 saveMu，串行）：节点删除后最多 60 秒磁盘就干净了。
// 读一次目录的成本远低于一次分片写，不必节流。
func (s *Store) pruneSeriesShards() int {
	dir := s.seriesDirPath()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	live := make(map[string]bool, len(ents))
	s.mu.RLock()
	for id := range s.nodes {
		live[shardFileName(id)] = true
	}
	s.mu.RUnlock()

	removed := 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".tmp-") {
			// 临时文件由写入方负责清理；这里只兜底清掉超过 1 小时的残留。
			if strings.HasPrefix(name, ".tmp-") {
				if info, ierr := e.Info(); ierr == nil && time.Since(info.ModTime()) > time.Hour {
					if os.Remove(filepath.Join(dir, name)) == nil {
						removed++
					}
				}
			}
			continue
		}
		if !strings.HasSuffix(name, ".json.gz") || live[name] {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed
}

// shardTask 是一次 Save 中待写分片的快照（锁内拷贝，锁外写盘）。
type shardTask struct {
	id  string
	seq uint64
	pts []model.Metrics
}

// collectShardTasks 在持读锁时收集"有新数据"的节点。
//
// 返回的 pts 必须来自 Ring.Slice()（已复制），否则解锁后与写入路径撞车。
func (s *Store) collectShardTasks() []shardTask {
	tasks := make([]shardTask, 0, len(s.series))
	for id, seq := range s.seriesSeq {
		if seq <= s.seriesSaved[id] {
			continue
		}
		r := s.series[id]
		if r == nil || r.n == 0 {
			continue
		}
		tasks = append(tasks, shardTask{id: id, seq: seq, pts: r.Slice()})
	}
	return tasks
}

// flushSeriesShards 写盘并推进已保存序号。
//
// 单个分片失败不中断其它分片与主快照：主快照是配置的唯一副本，
// 不能因为某台机器的历史写失败就整体放弃；失败的分片留在 seriesSeq 里，
// 下一次 Save 会重试（因为 saved < seq）。
func (s *Store) flushSeriesShards(tasks []shardTask, savedAt int64) (saveTiming, error) {
	var tm saveTiming
	if len(tasks) == 0 {
		return tm, nil
	}
	dir := s.seriesDirPath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tm, fmt.Errorf("创建历史分片目录 %s: %w", dir, err)
	}

	start := time.Now()
	var firstErr error
	for _, t := range tasks {
		n, err := writeSeriesShard(dir, t.id, t.pts, savedAt)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tm.SeriesBytes += n
		tm.SeriesFiles++
		s.mu.Lock()
		if s.seriesSaved[t.id] < t.seq {
			s.seriesSaved[t.id] = t.seq
		}
		s.mu.Unlock()
	}
	tm.Series = time.Since(start)
	return tm, firstErr
}
