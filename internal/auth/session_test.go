package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSessionRejectsOversizedToken 回归：
// 会话令牌来自客户端 Cookie，必须限制长度，否则一个接近 1MB 的 Cookie
// 就能逼服务端每个请求做一次 Split + 两次 base64 解码。
func TestSessionRejectsOversizedToken(t *testing.T) {
	s, err := NewSessions(bytes.Repeat([]byte{0x11}, 32), time.Hour)
	if err != nil {
		t.Fatalf("创建会话管理器失败: %v", err)
	}
	token, _, err := s.Issue("admin", 1)
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	if _, ok := s.Verify(token); !ok {
		t.Fatal("正常令牌应通过校验")
	}
	// 结构合法（两段、可 base64 解码）但远超上限：必须在做任何解码前拒绝。
	// 用耗时断言是因为"被拒绝"本身两种实现都成立，真正的差别是 CPU 成本：
	// 无上限时 400KB 令牌每次校验约 1ms，20 次约 20ms；有上限时立即返回。
	blob := strings.Repeat("A", 400*1024)
	huge := blob + "." + blob
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, ok := s.Verify(huge); ok {
			t.Fatal("超长令牌必须被拒绝")
		}
	}
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Fatalf("20 次超长令牌校验耗时 %v，说明没有长度上限（无上限约 20ms）", d)
	}
}

// TestSessionKeyConcurrentBootstrap 验证并发首启动只会产生一把密钥，
// 且所有实例拿到的都是完整密钥（不会读到半写文件）。
// 这是压力测试而非确定性回归：旧实现的窗口很窄，跑不出来不代表没缺陷。
func TestSessionKeyConcurrentBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = LoadOrCreateSessionKey(path)
		}(i)
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil {
			t.Fatalf("第 %d 个实例加载密钥失败: %v", i, errs[i])
		}
		if len(keys[i]) != sessionKeySize {
			t.Fatalf("第 %d 个实例拿到长度 %d 的密钥", i, len(keys[i]))
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("并发启动产生了多把密钥（会导致会话随机掉线）")
		}
	}
	// 目录里不应留下临时文件
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".session-key-") {
			t.Errorf("残留临时密钥文件: %s", e.Name())
		}
	}
}
