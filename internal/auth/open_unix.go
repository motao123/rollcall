//go:build unix

package auth

import (
	"os"
	"syscall"
)

// openNoFollow 以 O_NOFOLLOW 打开文件：即使路径在校验后被换成符号链接，
// 也不会跟随到目标文件，消除「lstat 后换链」的 TOCTOU 窗口。
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
