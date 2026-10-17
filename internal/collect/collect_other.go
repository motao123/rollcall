//go:build !linux

// 非 Linux 平台的占位实现：仅用于在本机（Windows/macOS）编译校验与联调。
// 生产部署目标是 Linux，实际采集逻辑见 collect_linux.go。
package collect

import (
	"runtime"

	"github.com/motao/probe/internal/model"
)

type Collector struct {
	host     model.HostInfo
	hostOnce bool
}

func New() *Collector { return &Collector{} }

func (c *Collector) Host() model.HostInfo {
	if !c.hostOnce {
		c.host = model.HostInfo{
			Hostname: "dev-host",
			OS:       runtime.GOOS + " (demo)",
			Arch:     runtime.GOARCH,
			Cores:    runtime.NumCPU(),
		}
		c.hostOnce = true
	}
	return c.host
}

// Collect 在非 Linux 平台返回零值，仅用于本地调试 UI。
func (c *Collector) Collect() model.Metrics {
	return model.Metrics{TS: timeNow()}
}
