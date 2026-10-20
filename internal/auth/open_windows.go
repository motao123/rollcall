//go:build windows

package auth

import (
	"os"
	"syscall"
)

// FILE_FLAG_OPEN_REPARSE_POINT：打开链接/重解析点本身，而不是它指向的目标。
// 打开后再检查属性，命中重解析点就拒绝 —— 等价于 Unix 的 O_NOFOLLOW。
const (
	_openReparsePoint   = 0x00200000
	_attributeReparsePt = 0x00000400
)

// openNoFollow 在 Windows 上以"打开重解析点本身 + 检查属性"实现 nofollow。
//
// 旧实现（open_other.go）在 Windows 上只是普通 os.Open，等价于跟随符号链接/
// junction/mount point：账号文件在校验之后被换成链接时，后续读取会落到
// 攻击者指定的目标上。
func openNoFollow(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|_openReparsePoint,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		_ = syscall.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&_attributeReparsePt != 0 {
		_ = syscall.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	return os.NewFile(uintptr(h), path), nil
}
