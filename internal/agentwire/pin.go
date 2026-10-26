package agentwire

// 服务端证书指纹固定（certificate pinning）。
//
// 背景：自签证书场景下 `tlsSkipVerify` 只做到「链路加密，但谁都能冒充」——
// 链路上的主动中间人可以用自己的自签证书接管上报通道，而上报内容里有节点
// 令牌与主机指纹。固定指纹把信任收敛到「安装时看到的那张证书」：仍然不依赖
// 公共 CA，但换证书必须显式改配置，中间人无法静默接管。
//
// 这不是完整 PKI：首次安装时若链路已被控制，指纹也会被一起替换（TOFU 的固有
// 局限）。因此面板会显示指纹供人工核对 —— 前提是安装者真的看一眼。

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// CertPinLen 是指纹字节数（SHA-256）。
const CertPinLen = 32

// ParseCertPin 解析十六进制证书指纹。
//
// 容忍常见书写差异（大写、冒号/空格/连字符分隔），但**长度必须精确**：
// 截断的指纹会让比较永远失败，这种错误必须在启动时报出来，而不是运行期
// 变成一个「永远连不上」的谜题。
func ParseCertPin(s string) ([]byte, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.NewReplacer(":", "", " ", "", "-", "").Replace(norm)
	if norm == "" {
		return nil, errors.New("证书指纹为空")
	}
	b, err := hex.DecodeString(norm)
	if err != nil {
		return nil, fmt.Errorf("证书指纹不是合法的十六进制: %w", err)
	}
	if len(b) != CertPinLen {
		return nil, fmt.Errorf("证书指纹应为 %d 字节（%d 个十六进制字符），实际 %d 字节",
			CertPinLen, CertPinLen*2, len(b))
	}
	return b, nil
}

// CertPinHex 返回证书的 SHA-256 指纹（小写十六进制）。
//
// 取的是**叶证书 DER**（rawCerts[0]），与 verifyPeerCertificate 的入参一致；
// 换成公钥或 SPKI 摘要会让两侧算出不同的值。
func CertPinHex(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// PinFromCertPEM 从 PEM 内容取第一张证书的指纹。
func PinFromCertPEM(b []byte) (string, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("未找到 CERTIFICATE PEM 块")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("解析证书失败: %w", err)
	}
	return CertPinHex(cert), nil
}

// PinFromCertFile 读证书文件并取指纹。
func PinFromCertFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return PinFromCertPEM(b)
}

// ClientTLSConfig 构造 Agent 的 TLS 客户端配置。
//
// 优先级：显式指纹 > 跳过校验 > 系统 CA。
//
// ★ 指纹格式错误时返回 error，**不退化**成「跳过校验」：
// 配置写错却继续跑，会让用户以为自己在固定指纹，实际仍在盲信任何证书。
func ClientTLSConfig(skipVerify bool, pinHex string) (*tls.Config, error) {
	pinHex = strings.TrimSpace(pinHex)
	if pinHex == "" {
		if skipVerify {
			return &tls.Config{InsecureSkipVerify: true}, nil
		}
		return nil, nil
	}
	pin, err := ParseCertPin(pinHex)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		// 自签证书没有可校验的信任链，身份改由下面的指纹比较保证。
		// 这里必须为 true：否则自签证书会在 CA 校验阶段先失败，
		// VerifyPeerCertificate 根本不会被调用。
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("服务端未提供证书")
			}
			got := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(got[:], pin) != 1 {
				return fmt.Errorf("服务端证书指纹不匹配：期望 %x，实际 %x（证书可能已更换；以面板显示的指纹为准）",
					pin, got)
			}
			return nil
		},
	}, nil
}
