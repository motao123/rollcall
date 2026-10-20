package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/motao123/rollcall/internal/store"
)

func readyStatus(h http.Handler) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func newReadyHandler(t *testing.T, dataDir string, saveEvery time.Duration) http.Handler {
	t.Helper()
	return New(store.New(t.TempDir()), nil, nil, Config{
		DataDir: dataDir, SaveInterval: saveEvery,
	}).Handler()
}

// TestReadyzHealthyAndPublic 确认刚启动、数据目录可写时 /readyz 返回 200，
// 且无需任何凭据（升级脚本在本机直接用 curl 调用）。
func TestReadyzHealthyAndPublic(t *testing.T) {
	dir := t.TempDir()
	h := newReadyHandler(t, dir, time.Minute)
	code, body := readyStatus(h)
	if code != http.StatusOK || body["ready"] != true {
		t.Fatalf("/readyz 应 200 ready=true，实际 %d %v", code, body)
	}
	if body["version"] == "" {
		t.Error("就绪响应应包含版本号，供升级脚本核对运行中的进程")
	}

	req := httptest.NewRequest(http.MethodPost, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("非 GET 应 405，实际 %d", w.Code)
	}
}

// TestReadyzDataDirNotWritable 数据目录消失/不可写时（磁盘卸载、权限变更）
// 进程仍能响应 /healthz，但必须通过 /readyz 报未就绪。
func TestReadyzDataDirNotWritable(t *testing.T) {
	h := newReadyHandler(t, filepath.Join(t.TempDir(), "unmounted"), time.Minute)
	code, body := readyStatus(h)
	if code != http.StatusServiceUnavailable || body["reason"] != "data dir not writable" {
		t.Fatalf("数据目录不可写应 503 data dir not writable，实际 %d %v", code, body)
	}
}

// TestReadyzNoSnapshotSaved 没有快照落盘时，超过两个保存周期就应报未就绪，
// 否则「进程活着但磁盘写不进去」会被升级脚本误判为成功。
func TestReadyzNoSnapshotSaved(t *testing.T) {
	h := newReadyHandler(t, t.TempDir(), 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	code, body := readyStatus(h)
	if code != http.StatusServiceUnavailable || body["reason"] != "no snapshot saved" {
		t.Fatalf("长时间没有快照应 503 no snapshot saved，实际 %d %v", code, body)
	}
}

// TestReadyzAfterSuccessfulSave 成功保存过快照后，即使超过启动宽限期仍应就绪。
func TestReadyzAfterSuccessfulSave(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	if err := st.Save(); err != nil {
		t.Fatalf("保存快照: %v", err)
	}
	h := New(st, nil, nil, Config{DataDir: dir, SaveInterval: 5 * time.Millisecond}).Handler()
	time.Sleep(30 * time.Millisecond)
	code, body := readyStatus(h)
	if code != http.StatusOK || body["ready"] != true {
		t.Fatalf("已有成功快照时应就绪，实际 %d %v", code, body)
	}
}
