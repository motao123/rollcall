//go:build linux

package logins

import (
	"io"
	"os"
	"time"
)

// wtmpPath 是登录历史文件。相对 /run/utmp（当前会话）而言，wtmp 才是
// "发生过哪些登录"的记录 —— 后者只保留当前会话，登出即消失。
const wtmpPath = "/var/log/wtmp"

// tailRecords 是每次读取的记录条数。
//
// 8 条 = 3KB，一次 read 就够；即使一个上报周期里连续有 8 个人登录（罕见），
// 也仍在上限内。读更多只会白白增加每个周期的磁盘读。
const tailRecords = 8

// readTail 读取 wtmp 末尾若干条记录。
//
// 只读尾部而不是整个文件：wtmp 可以有几十 MB（几年的记录），每 30 秒读一遍
// 是纯浪费。文件大小不是 384 的整数倍时（写入中断、轮转中）向下取整到记录
// 边界，避免把半条记录当数据解析出垃圾。
func readTail() ([]byte, time.Duration, error) {
	f, err := os.Open(wtmpPath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	age := time.Since(st.ModTime())
	size := st.Size()
	// 对齐到记录边界：从 newest 往下数，只取完整记录。
	end := size - size%recordSize
	if end <= 0 {
		return nil, age, nil
	}
	start := end - int64(tailRecords*recordSize)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, age, err
	}
	buf := make([]byte, end-start)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, age, err
	}
	return buf[:n], age, nil
}
