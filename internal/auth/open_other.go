//go:build !unix

package auth

import "os"

// openNoFollow 在无 O_NOFOLLOW 的平台上退回普通打开；
// 调用方仍会用 Lstat 拒绝符号链接。
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
