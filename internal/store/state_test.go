package store

import (
	"testing"

	"github.com/motao123/rollcall/internal/model"
)

func TestNodeStateBoundaries(t *testing.T) {
	const now int64 = 1_700_000_000

	cases := []struct {
		name     string
		lastSeen int64
		want     string
	}{
		{"从未上报", 0, model.NodeStateNoData},
		{"负时间戳", -1, model.NodeStateNoData},
		{"刚刚上报", now, model.NodeStateOnline},
		{"在线窗口边界", now - OnlineWindow, model.NodeStateOnline},
		{"刚超过在线窗口", now - OnlineWindow - 1, model.NodeStateStale},
		{"延迟窗口边界", now - StaleWindow, model.NodeStateStale},
		{"刚超过延迟窗口", now - StaleWindow - 1, model.NodeStateOffline},
		{"未来时间戳按在线处理", now + 30, model.NodeStateOnline},
	}
	for _, c := range cases {
		if got := NodeState(c.lastSeen, now); got != c.want {
			t.Errorf("%s: NodeState(%d) = %q，期望 %q", c.name, c.lastSeen, got, c.want)
		}
	}
}
