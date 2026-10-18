//go:build linux

package notify

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareProcess 让插件在独立进程组内运行，并在超时时回收整组。
//
// exec.CommandContext 默认只杀直接子进程：wrapper 脚本拉起的孙进程
// 会继续存活并持有 stdout/stderr 管道，反复告警会累积孤儿进程。
// 放进独立进程组后，向 -pid 发信号即可覆盖整棵树。
func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// 进程组已消失或权限不足时退回单进程，保证 Cancel 仍能收尾。
			return cmd.Process.Kill()
		}
		return nil
	}
}
