package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeExclusiveAtomic 原子发布一个「只允许首次创建」的文件。
//
// 为什么不能直接 O_EXCL 创建后写入：另一个并发启动的进程会在 O_EXCL
// 失败后立刻来读，此时目标文件可能还是 0 字节或半写，读方会看到
// "文件存在但内容非法"。先写临时文件再 Link 发布，读者要么看不到文件、
// 要么看到完整内容。
//
// 目标已存在时返回 os.ErrExist（调用方据此走"复用已有文件"的分支）。
func writeExclusiveAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, ".atomic-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	// Link 成功后临时名仍在，这里兜底清理；Rename 成功后路径已不存在，忽略错误。
	defer func() { _ = os.Remove(tmp) }()

	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("落盘临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		// 少数文件系统不支持硬链接：确认目标不存在后退回 rename。
		if _, statErr := os.Lstat(path); statErr == nil {
			return os.ErrExist
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("发布文件失败: %w", err)
		}
	}
	syncDir(dir)
	return nil
}
