//go:build !linux

package notify

import "os/exec"

// prepareProcess 在其他平台上保持默认行为（只结束直接子进程）。
// 进程组 API 是 Linux/Unix 语义，Windows 需要 Job Object，
// 而插件功能实际只部署在 Linux 服务端，这里不做伪实现。
func prepareProcess(cmd *exec.Cmd) {}

// cleanupProcess 在非 Linux 平台什么都不做：没有进程组，
// 只能依赖 exec 结束直接子进程。插件功能实际只部署在 Linux。
func cleanupProcess(cmd *exec.Cmd) {}
