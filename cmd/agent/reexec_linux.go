//go:build linux

package main

import (
	"os"
	"syscall"
)

// reexec 用 syscall.Exec 原地替换进程镜像：
// 不经过 shell、不 fork，systemd 仍能跟踪同一个主进程。
//
// 契约：args 必须是完整 argv（元素 0 为程序名），函数会无条件把 argv[0]
// 覆写为 path；若只传参数列表（不含程序名），首个参数会被丢弃。
// 现有调用方均传 os.Args 或 stripFlag(os.Args, ...)。
// 返回非 nil 表示新镜像并未启动，调用方不得继续执行旧进程
// （当前两个调用点都是 log.Fatalf，符合该约定）。
func reexec(path string, args []string) error {
	if len(args) == 0 {
		args = []string{path}
	}
	argv := make([]string, len(args))
	copy(argv, args)
	argv[0] = path
	return syscall.Exec(path, argv, os.Environ())
}
