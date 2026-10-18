//go:build linux

package main

import (
	"os"
	"syscall"
)

// reexec 用 syscall.Exec 原地替换进程镜像：
// 不经过 shell、不 fork，systemd 仍能跟踪同一个主进程。
func reexec(path string, args []string) error {
	if len(args) == 0 {
		args = []string{path}
	}
	argv := make([]string, len(args))
	copy(argv, args)
	argv[0] = path
	return syscall.Exec(path, argv, os.Environ())
}
