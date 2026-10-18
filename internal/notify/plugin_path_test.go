package notify

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckNoWritablePathRejectsIntermediate 是回归测试。
//
// 原来只检查最终可执行文件的「全局可写」位。command 位于子目录时，
// 中间目录若可被其他用户改写，攻击者能在「校验通过」与 exec 之间
// 把文件替换成任意程序 —— 等于本机任意代码执行。
func TestCheckNoWritablePathRejectsIntermediate(t *testing.T) {
	if !permChecksEnabled {
		t.Skip("当前平台不用 permission bits 判断可写性（Windows）")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := filepath.Join(sub, "cmd")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkNoWritablePath(root, cmd); err != nil {
		t.Fatalf("安全路径应通过: %v", err)
	}

	// 中间目录可被同组写：必须拒绝
	if err := os.Chmod(sub, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := checkNoWritablePath(root, cmd); err == nil {
		t.Fatal("中间目录可被同组改写时应拒绝")
	}

	// 其他用户可写同样拒绝
	if err := os.Chmod(sub, 0o702); err != nil {
		t.Fatal(err)
	}
	if err := checkNoWritablePath(root, cmd); err == nil {
		t.Fatal("中间目录可被其他用户改写时应拒绝")
	}
}
