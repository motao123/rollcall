package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/auth"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/notify"
)

// 本文件负责"非告警类"的运营通知：IP 变更、后台登录、流量校正留痕。
//
// 这些消息不走告警引擎：它们既没有阈值也没有持续时长，硬塞进规则体系
// 只会让规则列表变成杂物间。代价是要自己做去重，见 opNotifyCooldown。

// opNotifyCooldown 是同一类事件的最小提醒间隔。
//
// IP 变更是必需的：双线/VPN 出口的节点会在两三个地址之间反复横跳，
// 没有节流时一次切换就能刷出十几条消息。登录不做节流——管理员一天登录几次，
// 每条都值得看到；如果哪天觉得吵，应该加开关而不是悄悄丢消息。
const (
	ipChangeCooldown = 10 * time.Minute
	loginNotifyEvery = time.Duration(0)
)

// opNotify 返回 true 表示这次可以发（并记下时间），false 表示在冷却期内。
func (s *Server) opNotify(key string, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return true
	}
	now := time.Now().Unix()
	s.opNotifyMu.Lock()
	defer s.opNotifyMu.Unlock()
	if s.opNotifyAt == nil {
		s.opNotifyAt = make(map[string]int64)
	}
	if last, ok := s.opNotifyAt[key]; ok && now-last < int64(cooldown.Seconds()) {
		return false
	}
	// 顺手清掉过期条目，避免长期运行时 map 只增不减（节点数有限，成本极低）。
	if len(s.opNotifyAt) > 512 {
		for k, at := range s.opNotifyAt {
			if now-at >= int64(cooldown.Seconds()) {
				delete(s.opNotifyAt, k)
			}
		}
	}
	s.opNotifyAt[key] = now
	return true
}

// notifyIPChange 在节点上报来源地址变化时推送。
//
// 动态 IP 的家宽、切换线路、多出口的机房都会触发；消息里带旧值，
// 这样一眼能看出是"换了一个段"还是"完全换了地区"。
func (s *Server) notifyIPChange(n *model.Node, prev, next string) {
	if n == nil || prev == "" || next == "" || prev == next {
		return
	}
	if s.outbox == nil || !s.opNotify("ip:"+n.ID, ipChangeCooldown) {
		return
	}
	severity := "warn"
	// 首段不同（/24 之外）说明很可能换了机房或运营商，抬到 critical 更容易被看见。
	if firstOctet(prev) != firstOctet(next) {
		severity = "critical"
	}
	s.outbox.Enqueue(notify.Message{
		Title:    "节点上报地址变更",
		Body:     fmt.Sprintf("%s 的上报来源从 %s 变为 %s。若未做过迁移/换线，请检查该机器是否被接管。", n.Name, prev, next),
		Severity: severity,
		NodeName: n.Name,
		NodeID:   n.ID,
		Link:     s.panelLink,
		TS:       time.Now().Unix(),
	})
}

// notifyLogin 在面板登录成功后推送。
//
// 只报"谁、从哪来、用了哪种第二因素"，不报密码与令牌。
// 这条消息的价值在异常时：不是你登录的，你能立刻知道。
func (s *Server) notifyLogin(username, ip, method string) {
	if s.outbox == nil || !s.opNotify("login", loginNotifyEvery) {
		return
	}
	sev := "info"
	if s.trustedProxies != nil && isLoopbackIP(ip) {
		// 本机 / 隧道登录是最常见的安全形态，不必抬高噪音。
		sev = "info"
	}
	s.outbox.Enqueue(notify.Message{
		Title:    "面板登录",
		Body:     fmt.Sprintf("账号 %s 从 %s 登录（%s）。如果不是你本人操作，请立即改密并检查审计日志。", username, ip, method),
		Severity: sev,
		Link:     s.panelLink,
		TS:       time.Now().Unix(),
	})
}

// loginMethod 描述这次登录用了哪些因素，只写"有/没有"，不写任何凭据内容。
func loginMethod(u *auth.User) string {
	if u != nil && u.TOTPEnabled {
		return "密码 + 动态验证码"
	}
	return "密码"
}

func firstOctet(ip string) string {
	if i := strings.IndexByte(ip, '.'); i > 0 {
		return ip[:i]
	}
	// IPv6 取前两组，足够判断"是不是同一运营商/同一段"。
	parts := strings.Split(ip, ":")
	if len(parts) >= 2 {
		return parts[0] + ":" + parts[1]
	}
	return ip
}

func isLoopbackIP(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "127.")
}

// ---------- 流量校正留痕 ----------

// auditTrafficAdjust 记录一次人工校正，便于事后对账"谁把这台机器的用量改了"。
func (s *Server) auditTrafficAdjust(r *http.Request, nodeID string, before, after uint64) {
	actor, role := s.auditActor(r)
	s.st.AddAudit(model.AuditEntry{
		Action: "nodes.traffic-adjust",
		Actor:  actor,
		Role:   role,
		IP:     clientIP(r, s.trustedProxies),
		Method: r.Method,
		Path:   r.URL.Path,
		Status: http.StatusOK,
		Detail: fmt.Sprintf("%s: %d -> %d 字节", nodeID, before, after),
	})
}
