package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/motao123/rollcall/internal/agentwire"
	"github.com/motao123/rollcall/internal/model"
	"github.com/motao123/rollcall/internal/store"
)

// reportSecurityKey 是上报签名策略在设置表里的键。
const reportSecurityKey = "report-security"

// sigNonceCap 是 nonce 去重表的上限。窗口 90 秒、13 台 30 秒一报，
// 实际只会有几十条；这个上限是给"被大量伪造签名打"时兜底，
// 防止表被打爆。
const sigNonceCap = 4096

// reportSecurity 是上报签名策略。
//
// 默认 false（不强制）：升级服务端不应该让 13 台还在跑旧 Agent 的机器
// 立刻失联。正确顺序是先升 Agent（它们会开始签名，服务端忽略或采纳），
// 确认面板上全部显示"已签名"之后，再把这把开关打开。
type reportSecurity struct {
	RequireSignature bool `json:"requireSignature"`
}

func (s *Server) reportSecurity() reportSecurity {
	var cfg reportSecurity
	if raw := s.st.GetSetting(reportSecurityKey); raw != "" {
		// 解析失败按默认（不强制）处理：宁可少一层校验，也不要因为一个
		// 手改坏了的字符串让整机群上报全部 401。
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	return cfg
}

// checkReportSignature 校验一次上报的签名。
//
// 返回值 code 为 0 表示通过；否则是应当回给 Agent 的 HTTP 状态码。
// skew 是本次上报的时钟偏差（秒），仅在签名有效时可信。
func (s *Server) checkReportSignature(r *http.Request, node *model.Node, token string, body []byte) (code int, reason string, skew int64, signed bool) {
	tsRaw := strings.TrimSpace(r.Header.Get(agentwire.HeaderNodeTS))
	sig := strings.TrimSpace(r.Header.Get(agentwire.HeaderNodeSig))
	nonce := strings.TrimSpace(r.Header.Get(agentwire.HeaderNodeNonce))

	if sig == "" {
		if s.reportSecurity().RequireSignature {
			// 说清楚"该怎么修"，而不是只回一句 401：这条会被写进 Agent 日志，
			// 运维看到的第一行就是它。
			return 401, "本节点已要求签名上报，但这个 Agent 没有发送签名（升级到 v1.16.0+，或关闭「要求签名上报」）", 0, false
		}
		return 0, "", 0, false
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || ts <= 0 {
		return 401, "签名缺少有效的时间戳（" + agentwire.HeaderNodeTS + "）", 0, false
	}
	if nonce == "" || len(nonce) > 64 {
		return 401, "签名缺少有效的 nonce（" + agentwire.HeaderNodeNonce + "）", 0, false
	}

	now := time.Now().Unix()
	skew = ts - now
	if skew > agentwire.SigWindow || skew < -agentwire.SigWindow {
		// 这里不记录 skew 到台账：超窗的时间戳可能来自重放，不该被当成
		// "这台机器时钟偏了"写进去。真正要提示的偏差走下面的正常路径。
		return 401, "签名时间戳超出允许范围（±" + strconv.Itoa(agentwire.SigWindow) + " 秒），请检查本机时钟", skew, false
	}
	if !agentwire.VerifyReportSignature(token, ts, nonce, body, sig) {
		// 签名错可能意味着：令牌被轮换后 Agent 还在用旧的、body 被中间人改过、
		// 或者有人在伪造。三种都应立刻拒绝，且不回显"差在哪"。
		return 401, "上报签名校验失败（令牌不匹配或请求体被改动）", 0, false
	}
	if !s.acceptNonce(node.ID, nonce, now) {
		return 401, "重复的 nonce：这份上报已被处理过（可能是重放）", 0, false
	}
	return 0, "", skew, true
}

// acceptNonce 记录一个 nonce，返回 false 表示它在窗口内已经用过。
//
// 只用内存：这是"90 秒内不许重复"的短期状态，重启后丢失不会造成安全问题
// —— 攻击者要利用重启窗口，还得先拿到一个刚发出的签名请求。
func (s *Server) acceptNonce(nodeID, nonce string, now int64) bool {
	key := nodeID + "|" + nonce
	deadline := now + agentwire.SigWindow

	s.sigMu.Lock()
	defer s.sigMu.Unlock()
	if s.sigNonce == nil {
		s.sigNonce = make(map[string]int64, 64)
	}
	if _, seen := s.sigNonce[key]; seen {
		return false
	}
	if len(s.sigNonce) >= sigNonceCap {
		for k, exp := range s.sigNonce {
			if exp < now {
				delete(s.sigNonce, k)
			}
		}
		// 清完仍然满，说明正常上报量远超预期（或正在被打）。直接清空：
		// 少一层去重好过让新上报全部被拒。
		if len(s.sigNonce) >= sigNonceCap {
			s.sigNonce = make(map[string]int64, 64)
		}
	}
	s.sigNonce[key] = deadline
	return true
}

// handleAdminReportSecurity 读取/修改上报签名策略。
//
// GET 顺带返回"还有几台没签名"，因为这正是决定能不能打开开关的唯一依据：
// 让人对着数字判断，而不是凭印象。
func (s *Server) handleAdminReportSecurity(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeReportSecurity(w)
	case http.MethodPut, http.MethodPost:
		var in reportSecurity
		body := http.MaxBytesReader(w, r.Body, 4<<10)
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			fail(w, 400, "bad json: "+err.Error())
			return
		}
		raw, _ := json.Marshal(in)
		s.st.SetSetting(reportSecurityKey, string(raw))
		if in.RequireSignature {
			unsigned := s.unsignedNodeNames()
			if len(unsigned) > 0 {
				// 打开是真的会把没签名的节点踢下线（它们会一直 401），
				// 所以把名单回给前端，由界面提示"这会引起失联"。
				log.Printf("已要求签名上报，但仍有 %d 个节点未签名: %s", len(unsigned), strings.Join(unsigned, ", "))
			}
		}
		s.writeReportSecurity(w)
	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) writeReportSecurity(w http.ResponseWriter) {
	unsigned := s.unsignedNodeNames()
	writeJSON(w, 200, map[string]any{
		"requireSignature": s.reportSecurity().RequireSignature,
		"windowSeconds":    agentwire.SigWindow,
		"unsignedNodes":    unsigned,
		"signedCount":      s.signedNodeCount(),
	})
}

// unsignedNodeNames 列出还没发过签名上报的节点名。
//
// 只统计最近 OnlineWindow 内还有上报的节点：长期离线的机器不可能签名，
// 把它们算进来会让这个名单永远清不空，开关看起来就永远不能开。
func (s *Server) unsignedNodeNames() []string {
	out := []string{}
	now := time.Now().Unix()
	for _, n := range s.st.Nodes() {
		if n.Signed {
			continue
		}
		if _, ts, _ := s.st.Latest(n.ID); now-ts > store.OnlineWindow {
			continue
		}
		out = append(out, n.Name)
	}
	return out
}

func (s *Server) signedNodeCount() int {
	c := 0
	for _, n := range s.st.Nodes() {
		if n.Signed {
			c++
		}
	}
	return c
}
