//go:build !linux

package logins

import "errors"

// readTail 在非 Linux 平台直接报错。
//
// 控制端与机群都是 Linux；Windows/macOS 没有 wtmp（有各自的事件日志），
// 没有实机验证过的实现不写进来 —— Watcher 会把错误当成"没有新登录"，
// 不会影响上报的其它字段。
func readTail() ([]byte, error) {
	return nil, errors.New("登录事件采集仅在 Linux 可用")
}
