package outbox

import (
	"testing"
	"time"
)

// TestDefaultConfigHasDrainBudget 防止关停时排空预算退回成固定小常量。
//
// 单个 work 调用要串行遍历全部通道，每个 HTTP 通道就有 12 秒超时；
// 预算不足时 Stop 会先返回，调用方随即落盘并退出，
// 而在途投递随后才写库 —— 下次启动会重复投递同一条告警。
func TestDefaultConfigHasDrainBudget(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DrainTimeout < 20*time.Second {
		t.Fatalf("默认排空预算过短: %s", cfg.DrainTimeout)
	}
	if cfg.DrainTimeout > 60*time.Second {
		t.Fatalf("排空预算不应超过 systemd 默认 TimeoutStopSec: %s", cfg.DrainTimeout)
	}
}
