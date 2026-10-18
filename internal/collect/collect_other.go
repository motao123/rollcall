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

func (c *Collector) Host() model.HostInfo {
	c.hostOnce.Do(func() {
		hostname := "dev-host"
		// 用真实主机名，避免联调时节点在面板里显示成假名字、污染资产台账。
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
