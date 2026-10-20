//go:build !unix && !windows

package auth

import "os"

// openNoFollow 在既非 Unix 也非 Windows 的平台上退回普通打开；
// 调用方仍会用 Lstat 拒绝符号链接（TOCTOU 窗口在此类平台无法彻底消除）。
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
