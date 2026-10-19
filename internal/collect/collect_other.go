//go:build !linux

// 非 Linux 平台的占位实现：仅用于在本机（Windows/macOS）编译校验与联调。
// 生产部署目标是 Linux，实际采集逻辑见 collect_linux.go。
package collect

import (
	"os"
	"runtime"
	"sync"

	"github.com/motao/rollcall/internal/model"
)

type Collector struct {
	hostOnce sync.Once
	host     model.HostInfo
}

func New() *Collector { return &Collector{} }

// Supported 表示当前平台是否有真实采集实现。
// Agent 在非 Linux 上据此拒绝上报全零指标，而不是把它们当成真实数据。
func Supported() bool { return false }

func (c *Collector) Host() model.HostInfo {
	c.hostOnce.Do(func() {
		// 查不到主机名时用 unknown，不要编造一个看起来像真名的 dev-host。
		hostname := "unknown"
		if hn, err := os.Hostname(); err == nil && hn != "" {
			hostname = hn
		}
		c.host = model.HostInfo{
			Hostname: hostname,
			OS:       runtime.GOOS + " (demo)",
			Arch:     runtime.GOARCH,
			Cores:    runtime.NumCPU(),
		}
	})
	return c.host
}

// Collect 在非 Linux 平台返回零值，仅用于本地调试 UI。
// agent 启动时已明确警告该平台不会采集真实指标。
func (c *Collector) Collect() model.Metrics {
	return model.Metrics{TS: timeNow()}
}
