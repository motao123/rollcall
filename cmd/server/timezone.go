package main

// 时区。
//
// 面板里所有"按天/按小时"的行为都依赖它：日报周报的发送时刻、免打扰窗口、
// 流量周期的每月起始日、到期提醒的"当天"。服务器默认往往是 UTC（容器更是），
// 于是"每天 9 点日报"在西八区之外的部署上会变成另一个时刻，而界面上看不出来。
//
// Nezha 的 config.yaml 里有显式 `location`，同一类产品都绕不开这个问题；
// 这里用 `-timezone`（IANA 名）或环境变量 TZ 明确指定，并把生效值显示在设置页。

import (
	"fmt"
	"strings"
	"time"

	// 内嵌时区数据库：distroless/精简镜像里通常没有 /usr/share/zoneinfo，
	// 不带这份数据时 LoadLocation 会直接失败（而静态二进制多 400KB 无所谓）。
	_ "time/tzdata"
)

// applyTimezone 设置进程级时区并返回生效值。
//
// 必须在启动早期调用，且只调用一次：`time.Local` 是进程级全局，运行期改它
// 与其它 goroutine 的读取存在数据竞争，所以我们只允许在启动时确定。
func applyTimezone(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Local.String(), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", fmt.Errorf("无效的时区 %q（需 IANA 名，如 Asia/Shanghai）: %w", name, err)
	}
	time.Local = loc
	return loc.String(), nil
}
