package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeELF 构造带合法 ELF 头的最小文件；Apply 会校验魔数与架构。
func fakeELF(marker string) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4] = 2 // 64 位
	b[5] = 1 // 小端
	machine := map[string]uint16{"amd64": 62, "arm64": 183, "386": 3, "arm": 40}[runtime.GOARCH]
	binary.LittleEndian.PutUint16(b[18:20], machine)
	copy(b[20:], marker)
	return b
}

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	return pub, priv
}

func envelopeJSON(t *testing.T, payload any, priv ed25519.PrivateKey) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 payload: %v", err)
	}
	env := envelope{
		Schema:    1,
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("序列化信封: %v", err)
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVerifyEnvelope(t *testing.T) {
	pub, priv := testKey(t)
	body := bytes.Repeat([]byte("x"), 2048)
	p := payload{
		Version: "1.7.0", PublishedAt: 1700000000,
		Assets: map[string]Asset{
			"linux-amd64": {URL: "https://example.com/a", SHA256: sha(body), Size: int64(len(body))},
		},
	}
	raw := envelopeJSON(t, p, priv)

	m, err := VerifyEnvelope(raw, pub)
	if err != nil {
		t.Fatalf("合法信封应通过: %v", err)
	}
	if m.Version != "1.7.0" || m.Assets["linux-amd64"].Size != int64(len(body)) {
		t.Fatalf("清单解析异常: %+v", m)
	}

	// 篡改 payload / 换公钥 / 错误 schema 都必须失败。
	otherPub, _ := testKey(t)
	if _, err := VerifyEnvelope(raw, otherPub); err == nil {
		t.Fatal("错误公钥应验签失败")
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	bad := env
	bad.Schema = 2
	b, _ := json.Marshal(bad)
	if _, err := VerifyEnvelope(b, pub); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("高 schema 应拒绝: %v", err)
	}
	bad = env
	payloadRaw, _ := base64.StdEncoding.DecodeString(env.Payload)
	payloadRaw[0] ^= 0xff
	bad.Payload = base64.StdEncoding.EncodeToString(payloadRaw)
	b, _ = json.Marshal(bad)
	if _, err := VerifyEnvelope(b, pub); err == nil {
		t.Fatal("被篡改的 payload 应验签失败")
	}

	// 资产字段校验。
	cases := []map[string]Asset{
		{"linux-amd64": {URL: "https://x/a", SHA256: "nothex", Size: 2048}},
		{"linux-amd64": {URL: "https://x/a", SHA256: sha(body), Size: 1}},
		{"bad key": {URL: "https://x/a", SHA256: sha(body), Size: 2048}},
		{"linux-amd64": {SHA256: sha(body), Size: 2048}},
	}
	for _, assets := range cases {
		raw := envelopeJSON(t, payload{Version: "1.7.0", Assets: assets}, priv)
		if _, err := VerifyEnvelope(raw, pub); err == nil {
			t.Fatalf("非法资产应拒绝: %+v", assets)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		remote, current string
		want            bool
	}{
		{"1.7.0", "1.6.1", true},
		{"v1.7.0", "1.6.1", true},
		{"1.6.1", "1.6.1", false},
		{"1.6.0", "1.6.1", false},
		{"1.7.0", "dev", true},
		{"1.7.0-rc1", "1.7.0", false},
		{"1.7.0", "1.7.0-rc1", true},
		{"1.10.0", "1.9.9", true},
		{"bad", "1.0.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.remote, c.current); got != c.want {
			t.Fatalf("IsNewer(%q,%q)=%v want %v", c.remote, c.current, got, c.want)
		}
	}
	same := []struct{ a, b string }{
		{"v1.7.0", "1.7.0"},
		{"1.7.0+build.5", "v1.7.0"},
		{"v1.7.0-rc1", "1.7.0-rc1"},
	}
	for _, c := range same {
		if !SameVersion(c.a, c.b) {
			t.Fatalf("SameVersion(%q,%q) 应为 true", c.a, c.b)
		}
	}
	if SameVersion("1.7.0", "1.7.1") || SameVersion("dev", "dev") {
		t.Fatal("不同版本或不可解析版本不应视为相同")
	}
}

func TestUpdaterFetchAndDownload(t *testing.T) {
	pub, priv := testKey(t)
	body := bytes.Repeat([]byte("agent-binary"), 256)
	p := payload{
		Version: "2.0.0", PublishedAt: 1700000000,
		Assets: map[string]Asset{
			"linux-amd64": {URL: "https://example.com/agent", SHA256: sha(body), Size: int64(len(body))},
		},
	}
	envRaw := envelopeJSON(t, p, priv)

	var serveBody = body
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(envRaw)
	})
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(serveBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 测试服务器在回环地址上，HTTP 无需 allow-insecure。
	u, err := New(srv.URL+"/manifest", base64.StdEncoding.EncodeToString(pub), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u.GOOS, u.GOARCH = "linux", "amd64"
	m, err := u.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	asset, newer := u.Candidate(m, "1.9.0")
	if !newer {
		t.Fatal("应判定有更新")
	}
	asset.URL = srv.URL + "/asset"
	dir := t.TempDir()
	path, err := u.Download(context.Background(), asset, dir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("下载内容不符: err=%v len=%d", err, len(got))
	}

	// 哈希错误
	bad := asset
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := u.Download(context.Background(), bad, dir); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("哈希错误应拒绝: %v", err)
	}
	// 大小不符
	serveBody = body[:100]
	if _, err := u.Download(context.Background(), asset, dir); err == nil || !strings.Contains(err.Error(), "大小不符") {
		t.Fatalf("大小不符应拒绝: %v", err)
	}
	serveBody = body

	// 不支持的平台
	u.GOARCH = "mips"
	if _, newer := u.Candidate(m, "1.9.0"); newer {
		t.Fatal("缺少平台资产时不应有更新")
	}
}

func TestUpdaterURLPolicy(t *testing.T) {
	pub, _ := testKey(t)
	key := base64.StdEncoding.EncodeToString(pub)
	if _, err := New("http://example.com/manifest.json", key, false); err == nil {
		t.Fatal("公网明文清单地址应拒绝")
	}
	if _, err := New("http://example.com/manifest.json", key, true); err != nil {
		t.Fatalf("显式 allow-insecure 应放行: %v", err)
	}
	if _, err := New("http://127.0.0.1:8080/manifest.json", key, false); err != nil {
		t.Fatalf("回环地址应放行: %v", err)
	}
	if _, err := New("ftp://example.com/x", key, true); err == nil {
		t.Fatal("非 http(s) 应拒绝")
	}
	if _, err := New("https://example.com/x", "not-base64", true); err == nil {
		t.Fatal("非法公钥应拒绝")
	}
}

func TestApplyStartupRollback(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "rollcall-agent")
	oldBody := fakeELF("old")
	newBody := fakeELF("new")
	if err := os.WriteFile(exe, oldBody, 0o755); err != nil {
		t.Fatal(err)
	}
	down := filepath.Join(dir, "new")
	if err := os.WriteFile(down, newBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(exe, down, "v2.0.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, newBody) {
		t.Fatalf("exe 未替换: %q", b)
	}
	if b, _ := os.ReadFile(BackupPath(exe)); !bytes.Equal(b, oldBody) {
		t.Fatalf("备份不符: %q", b)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("新版本缺少可执行权限: %v", err)
	}

	for i := 0; i < MaxStartupAttempts-1; i++ {
		// 构建版本带 v 前缀、清单存储不带；必须按语义匹配。
		rolled, err := HandleStartup(exe, "2.0.0")
		if err != nil || rolled {
			t.Fatalf("第 %d 次启动不应回滚: rolled=%v err=%v", i+1, rolled, err)
		}
	}
	rolled, err := HandleStartup(exe, "2.0.0")
	if err != nil || !rolled {
		t.Fatalf("第 %d 次启动应回滚: rolled=%v err=%v", MaxStartupAttempts, rolled, err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, oldBody) {
		t.Fatalf("回滚后 exe 应为旧版本: %q", b)
	}
	if v, ok := BlockedVersion(exe); !ok || v != "2.0.0" {
		t.Fatalf("回滚版本未进入冷却: %q %v", v, ok)
	}

	// 非 ELF 更新包必须在替换前被拒绝。
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not-an-elf-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(exe, bad, "2.1.0"); err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("非 ELF 更新包应拒绝: %v", err)
	}
}

func TestManualRollbackAndStable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	v1 := fakeELF("v1")
	_ = os.WriteFile(exe, v1, 0o755)
	down := filepath.Join(dir, "new")
	_ = os.WriteFile(down, fakeELF("v2"), 0o755)
	if err := Apply(exe, down, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := ManualRollback(exe, "v2.0.0"); err != nil {
		t.Fatalf("ManualRollback: %v", err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, v1) {
		t.Fatalf("手动回滚失败: %q", b)
	}
	if v, ok := BlockedVersion(exe); !ok || v != "2.0.0" {
		t.Fatalf("手动回滚未记录冷却: %q %v", v, ok)
	}

	// 新版本成功上报后清除标记，不再自动回滚。
	down2 := filepath.Join(dir, "new2")
	_ = os.WriteFile(down2, fakeELF("v3"), 0o755)
	if err := Apply(exe, down2, "3.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := MarkStable(exe); err != nil {
		t.Fatalf("MarkStable: %v", err)
	}
	if _, err := os.Stat(StatePath(exe)); !os.IsNotExist(err) {
		t.Fatal("稳定后标记应删除")
	}

	// 更新包必须与可执行文件同目录。
	other := t.TempDir()
	cross := filepath.Join(other, "new")
	_ = os.WriteFile(cross, fakeELF("v4"), 0o755)
	if err := Apply(exe, cross, "4.0.0"); err == nil {
		t.Fatal("跨目录更新包应拒绝")
	}
}
