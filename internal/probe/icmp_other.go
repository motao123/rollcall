//go:build !linux

package probe

import (
	"errors"
	"net"
	"time"
)

// pingOnce 在非 Linux 平台直接报错。
//
// 控制端生产环境只有 Linux；Windows/macOS 上 raw ICMP 的权限模型与
// Linux 完全不同，没有实机验证过的实现不写进来 —— 明确失败好过
// "看起来支持、实际永远超时"。
func pingOnce(net.IP, time.Duration) (time.Duration, error) {
	return 0, errors.New("ICMP 探测仅在 Linux 控制端可用")
}
