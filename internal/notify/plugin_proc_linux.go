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
		// Go 1.24 的 Process 通过 pidfd 追踪子进程：已被 Wait 回收时
		// Signal 返回 ErrProcessDone。必须先做这个检查，再向 -pid 发信号；
		// 否则延迟到达的 Cancel 可能命中 PID 复用后的无关进程组。
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return err
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// 进程组已消失或权限不足时退回单进程，保证 Cancel 仍能收尾。
			return cmd.Process.Kill()
		}
		return nil
	}
}

// cleanupProcess 在 Run 返回后回收进程组中的残留孙进程。
// 组内仍有成员时 PGID 不会被内核复用，因此这里按 -pid 补刀不会
// 命中无关进程；组已空时 kill 返回 ESRCH，忽略即可。
func cleanupProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
