//go:build unix

package notify

import (
	"fmt"
	"os"
	"syscall"
)

// checkPathOwner 要求插件路径属于当前用户或 root。
//
// 只检查 022 权限位并不够：0755 的目录只要属主是另一个本地用户，
// 对方就能随意替换文件；uid 比对才能排除这种「看起来只读」的路径。
func checkPathOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() && st.Uid != 0 {
		return fmt.Errorf("%s 属于其他用户（uid %d），拒绝加载插件", path, st.Uid)
	}
	return nil
}
