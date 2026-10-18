package selfupdate

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// MaxStartupAttempts 是新版本允许的启动次数；达到后自动回滚 .prev。
	MaxStartupAttempts = 3
	// FailedVersionBackoff 是回滚后不再自动尝试同一版本的时间窗口。
	FailedVersionBackoff = 7 * 24 * time.Hour
)

// updateState 记录一次待验证的更新。
// RolledBack=true 表示该版本启动失败已回滚，仅用于阻止同一版本反复安装。
type updateState struct {
	Version     string `json:"version"`
	Attempts    int    `json:"attempts"`
	InstalledAt int64  `json:"installedAt,omitempty"`
	RolledBack  bool   `json:"rolledBack,omitempty"`
	FailedAt    int64  `json:"failedAt,omitempty"`
}

func StatePath(exe string) string  { return exe + ".update-pending" }
func BackupPath(exe string) string { return exe + ".prev" }

func readState(exe string) (*updateState, error) {
	b, err := os.ReadFile(StatePath(exe))
	if err != nil {
		return nil, err
	}
	var st updateState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("更新状态文件损坏: %w", err)
	}
	return &st, nil
}

func writeState(exe string, st *updateState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".rollcall-agent-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, StatePath(exe)); err != nil {
		_ = os.Remove(name)
		return err
	}
	syncDir(dir)
	return nil
}

// HandleStartup 在 main 的最开始调用：
//   - 新版本连续启动 MaxStartupAttempts 次仍未进入稳定状态时，用 .prev 回滚
//   - 返回 true 表示可执行文件已被替换，调用方应立即 re-exec
//
// 标记损坏或版本不匹配时直接清理，不阻塞 Agent 启动。
func HandleStartup(exe, currentVersion string) (bool, error) {
	st, err := readState(exe)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		_ = os.Remove(StatePath(exe))
		return false, nil
	}
	if st.RolledBack {
		return false, nil
	}
	if !SameVersion(st.Version, currentVersion) {
		_ = os.Remove(StatePath(exe))
		return false, nil
	}
	st.Attempts++
	if st.Attempts < MaxStartupAttempts {
		return false, writeState(exe, st)
	}
	if err := restoreBackup(exe); err != nil {
		// 没有可用备份时清掉标记，避免每次启动都重复失败。
		_ = os.Remove(StatePath(exe))
		return false, fmt.Errorf("更新回滚失败: %w", err)
	}
	st.Attempts = 0
	st.RolledBack = true
	st.FailedAt = time.Now().Unix()
	return true, writeState(exe, st)
}

// MarkStable 在首次成功上报后调用，删除待验证标记。
// 回滚后写入的标记会保留，用于 FailedVersionBackoff。
func MarkStable(exe string) error {
	st, err := readState(exe)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		_ = os.Remove(StatePath(exe))
		return nil
	}
	if st.RolledBack {
		return nil
	}
	return os.Remove(StatePath(exe))
}

// BlockedVersion 返回回滚后仍在冷却期内的版本。
func BlockedVersion(exe string) (string, bool) {
	st, err := readState(exe)
	if err != nil || !st.RolledBack || st.FailedAt == 0 {
		return "", false
	}
	if time.Since(time.Unix(st.FailedAt, 0)) >= FailedVersionBackoff {
		return "", false
	}
	return st.Version, true
}

// Apply 备份当前可执行文件、写入启动标记并原子替换为新版本。
// downloaded 必须与 exe 同目录（Download 已保证）。
func Apply(exe, downloaded, newVersion string) error {
	if _, ok := parseVersion(newVersion); !ok {
		return fmt.Errorf("更新版本号无效: %q", newVersion)
	}
	newVersion = canonicalVersion(newVersion)
	dir := filepath.Dir(exe)
	if filepath.Dir(downloaded) != dir {
		return errors.New("更新包必须与可执行文件位于同一目录")
	}
	fi, err := os.Stat(exe)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("当前可执行文件不可用: %w", err)
	}
	nf, err := os.Stat(downloaded)
	if err != nil || !nf.Mode().IsRegular() {
		return fmt.Errorf("更新包不可用: %w", err)
	}
	if err := checkELF(downloaded); err != nil {
		return err
	}
	perm := fi.Mode().Perm()
	if perm&0o111 == 0 {
		perm |= 0o700
	}

	// 先做一份独立备份；后续 rename 覆盖 exe 时不会出现空窗。
	bakTmp, err := copyToTemp(exe, dir, perm)
	if err != nil {
		return fmt.Errorf("备份当前版本失败: %w", err)
	}
	if err := os.Rename(bakTmp, BackupPath(exe)); err != nil {
		_ = os.Remove(bakTmp)
		return fmt.Errorf("写入备份失败: %w", err)
	}

	// 标记必须在替换前落盘，否则崩溃后无法统计启动次数。
	if err := writeState(exe, &updateState{
		Version: newVersion, InstalledAt: time.Now().Unix(),
	}); err != nil {
		return fmt.Errorf("写入更新标记失败: %w", err)
	}
	if err := os.Chmod(downloaded, perm); err != nil {
		return fmt.Errorf("设置新版本权限失败: %w", err)
	}
	if err := os.Rename(downloaded, exe); err != nil {
		return fmt.Errorf("替换可执行文件失败: %w", err)
	}
	syncDir(dir)
	return nil
}

// ManualRollback 把 .prev 恢复为当前可执行文件，并记录失败版本冷却。
func ManualRollback(exe, failedVersion string) error {
	if err := restoreBackup(exe); err != nil {
		return err
	}
	return writeState(exe, &updateState{
		Version: canonicalVersion(failedVersion), RolledBack: true, FailedAt: time.Now().Unix(),
	})
}

func restoreBackup(exe string) error {
	bak := BackupPath(exe)
	fi, err := os.Stat(bak)
	if err != nil || !fi.Mode().IsRegular() {
		return errors.New("没有可用的上一版本备份（.prev）")
	}
	perm := fi.Mode().Perm()
	if perm&0o111 == 0 {
		perm |= 0o700
	}
	tmp, err := copyToTemp(bak, filepath.Dir(exe), perm)
	if err != nil {
		return fmt.Errorf("复制备份失败: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("恢复备份失败: %w", err)
	}
	syncDir(filepath.Dir(exe))
	return nil
}

func copyToTemp(src, dir string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".rollcall-agent-copy-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// checkELF 在替换前拒绝明显错误架构/格式的更新包。
// 否则一次错误签名会让 systemd 反复 exec 失败，而新二进制根本没机会计数回滚。
func checkELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("读取更新包失败: %w", err)
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return errors.New("更新包过小，不是有效的 ELF")
	}
	if hdr[0] != 0x7f || hdr[1] != 'E' || hdr[2] != 'L' || hdr[3] != 'F' {
		return errors.New("更新包不是 ELF 可执行文件")
	}
	if hdr[5] != 1 { // EI_DATA=litte endian
		return errors.New("更新包不是小端 ELF")
	}
	machine := binary.LittleEndian.Uint16(hdr[18:20])
	want, ok := map[string]uint16{
		"amd64": 62, "arm64": 183, "386": 3, "arm": 40,
	}[runtime.GOARCH]
	if ok && machine != want {
		return fmt.Errorf("更新包架构不匹配：ELF machine %d，当前 %s", machine, runtime.GOARCH)
	}
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
