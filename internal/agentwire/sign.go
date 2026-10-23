// Package agentwire 的上报签名契约。
//
// 背景：上报原先只靠一个静态 X-Node-Token。能抓到一次上报的人（同一台
// 机器上的其他进程、中间链路、日志镜像）就可以无限重放，或者用自造
// 的 JSON 冒充该节点 —— 而指标异常会直接推进告警，最终推到 Telegram/
// 企微。也就是说，一条伪造的上报可以让值班的人在凌晨被叫起来。
//
// 方案是给每份上报加一个短期凭据：
//
//	X-Node-Ts   : 单位秒的 Unix 时间
//	X-Node-None : 每次上报随机的 nonce
//	X-Node-Sig  : HMAC-SHA256(token, "v1\nts\nnonce\nsha256(body)")
//
// 服务端只接受时间落在 ±SigWindow 内的上报（挡住"录下来过一小时再放"
// 这类重放），并用 nonce 去重挡住窗口内的立即重放。
//
// 为什么签名用 token 而不是再发一把密钥：token 已经是"只有该节点和
// 主控知道"的共享秘密，单独再加一把只增加分发与轮换成本，不增加安全
// 边界。真要区分"能上报"与"能验签"两种权限时再拆。
//
// 为什么 canonical 里带 sha256(body) 而不是直接签 body：签名头有长度
// 上限（实际受 HTTP 头大小限制），而 body 可达 64KB。先摘要再签是
// HMAC 的标准用法，等价强度。
package agentwire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

const (
	// HeaderNodeTS / HeaderNodeNonce / HeaderNodeSig 是签名三元组。
	HeaderNodeTS    = "X-Node-Ts"
	HeaderNodeNonce = "X-Node-Nonce"
	HeaderNodeSig   = "X-Node-Sig"

	// SigVersion 是签名算法版本，参与 canonical 计算。
	// 一旦换算法，旧 Agent 的签名会因为前缀不同而直接被拒，
	// 而不是"算出来对不上"这种难以归因的失败。
	SigVersion = "v1"

	// SigWindow 是允许的时钟偏差（秒）。取 90 秒：
	// 大多数 NTP 同步误差在毫秒级，90 秒足够容忍一次失败的同步；
	// 再放宽就等于给重放留出更长的可用窗口。
	SigWindow = 90

	// NonceLen 是 nonce 的十六进制长度（16 字节）。
	NonceLen = 32
)

// ReportSignature 计算一份上报的签名（十六进制小写）。
//
// 调用方必须传入**实际发送的 body 原始字节**，而不是重新序列化一次
// 的结果：JSON 的 map 顺序与浮点格式不保证稳定，重新序列化会得到
// 不同字节、签名永远对不上。
func ReportSignature(token string, ts int64, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(SigVersion))
	mac.Write([]byte("\n"))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write([]byte(hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyReportSignature 校验签名。用 hmac.Equal 做常数时间比较：
// 逐字节比较会通过耗时差异泄漏"前几位对了"，足以在本地网络里爆破。
func VerifyReportSignature(token string, ts int64, nonce string, body []byte, sig string) bool {
	want := ReportSignature(token, ts, nonce, body)
	return hmac.Equal([]byte(want), []byte(sig))
}
