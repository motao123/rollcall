package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
	"github.com/motao123/rollcall/internal/sanitize"
)

// loginSeenCap 是登录去重表的上限。
//
// 正常情况每个节点一天几条，几十条就是上限级别；这个数字是给"被大量
// 登录刷屏"时兜底，避免表无界增长。
const loginSeenCap = 4096

// handleNodeLogin 记录一次交互式登录并按需通知。
//
// 所有字段都来自被监控节点（Agent 读的是 wtmp，而 wtmp 里的用户名与来源
// 主机是**登录者自己决定的**），因此必须当成不可信输入：先 sanitize 再
// 入库，且长度截断。否则一条带控制字符或超长串的 host 会污染通知与面板。
func (s *Server) handleNodeLogin(n *model.Node, ev model.LoginEvent) {
	if n == nil || ev.TS <= 0 {
		return
	}
	ev.User = sanitize.Text(strings.TrimSpace(ev.User), 32)
	ev.Host = sanitize.Text(strings.TrimSpace(ev.Host), 64)
	ev.Line = sanitize.Text(strings.TrimSpace(ev.Line), 32)
	if ev.User == "" {
		return
	}
	// 未来时间戳直接拒绝：它会让 LastLogin 永久"更新"，之后所有正常事件
	// 都被判定为旧事件而不再通知。Agent 时钟错乱或伪造都可能触发。
	if ev.TS > time.Now().Unix()+300 {
		return
	}

	if !s.acceptLogin(n.ID, ev) {
		return
	}
	s.st.SetNodeLastLogin(n.ID, ev)

	if s.outbox == nil {
		return
	}
	from := ev.Host
	if from == "" {
		from = "本地控制台"
	}
	sev := "info"
	if strings.EqualFold(ev.User, "root") && ev.Host != "" {
		// root 从网络登录是最值得看一眼的形态；本地 root（控制台/救援）不抬。
		sev = "warn"
	}
	line := ev.Line
	if line == "" {
		line = "未知终端"
	}
	s.outbox.Enqueue(notify.Message{
		Title:    "节点登录",
		Body:     fmt.Sprintf("%s 上有新登录：%s 从 %s（%s）。若不是你本人操作，请检查该机器是否被接管。", n.Name, ev.User, from, line),
		Severity: sev,
		NodeName: n.Name,
		NodeID:   n.ID,
		Link:     s.panelLink,
		TS:       ev.TS,
	})
}

// acceptLogin 记录一条登录的签名，返回 false 表示它已经处理过。
func (s *Server) acceptLogin(nodeID string, ev model.LoginEvent) bool {
	key := fmt.Sprintf("%s|%d|%s|%s", nodeID, ev.TS, ev.User, ev.Host)
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginSeen == nil {
		s.loginSeen = make(map[string]struct{}, 64)
	}
	if _, ok := s.loginSeen[key]; ok {
		return false
	}
	if len(s.loginSeen) >= loginSeenCap {
		// 满了就整体清空：少一层去重好过让新登录全部被忽略。
		s.loginSeen = make(map[string]struct{}, 64)
	}
	s.loginSeen[key] = struct{}{}
	return true
}
