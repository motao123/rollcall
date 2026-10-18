package main

import (
	"math"
	"math/rand"
	"time"

	"github.com/motao/rollcall/internal/model"
	"github.com/motao/rollcall/internal/store"
)

// seedDemo 生成一组贴近真实的演示数据，用于预览 UI 而不必先部署 Agent。
// 仅在 -demo 参数下调用，生产环境不会执行。
func seedDemo(st *store.Store) {
	type spec struct {
		id, name, provider, region, group, expire, os string
		price                                         float64
		cur                                           string
		baseCPU, baseMem                              float64
		disk                                          float64
	}

	specs := []spec{
		{"n01", "腾讯云-sg", "腾讯云", "新加坡", "prod", "2030-03-07", "Debian 12", 0, "CNY", 22, 38, 58},
		{"n02", "腾讯云-上海", "腾讯云", "上海", "prod", "2027-06-12", "Debian 13", 0, "CNY", 34, 55, 71},
		{"n03", "阿里云-成都", "阿里云", "成都", "prod", "2027-01-20", "Ubuntu 22.04", 0, "CNY", 18, 44, 62},
		{"n04", "华为云-香港", "华为云", "香港", "prod", "2026-12-01", "Ubuntu 22.04", 0, "CNY", 27, 41, 49},
		{"n05", "netcup-1o", "netcup", "德国", "probe", "2027-03-15", "Debian 12", 6.5, "USD", 12, 31, 34},
		{"n06", "netcup-RS1000", "netcup", "德国", "prod", "2027-08-02", "Debian 12", 14.0, "USD", 41, 63, 77},
		{"n07", "racknerd-圣何塞", "racknerd", "美国", "probe", "2027-02-18", "Debian 12", 11.0, "USD", 8, 22, 28},
		{"n08", "racknerd-纽约", "racknerd", "美国", "probe", "2026-11-09", "Debian 12", 11.0, "USD", 16, 37, 41},
		{"n09", "racknerd-DC02", "racknerd", "美国", "test", "2026-10-28", "Debian 12", 9.5, "USD", 5, 19, 23},
		{"n10", "DMIT-49.9", "DMIT", "美国", "prod", "2027-05-30", "Debian 12", 49.9, "USD", 33, 52, 66},
		{"n11", "dmit-wee", "DMIT", "美国", "prod", "2027-04-11", "Debian 12", 36.9, "USD", 45, 58, 72},
		{"n12", "棉花云-香港", "棉花云", "香港", "probe", "2026-12-25", "Debian 12", 0, "CNY", 9, 26, 31},
		{"n13", "bestvm.cloud-tw", "bestvm", "台湾", "probe", "2027-07-19", "Debian 12", 18.0, "USD", 14, 29, 38},
		{"n14", "hostyun-us", "hostyun", "美国", "test", "2026-11-30", "CentOS 7", 7.0, "USD", 3, 17, 21},
		{"n15", "武汉-独服", "自建", "武汉", "prod", "2029-01-01", "Ubuntu 24.04", 0, "CNY", 52, 68, 88},
	}

	now := time.Now().Unix()
	rnd := rand.New(rand.NewSource(20261017))

	// 离线演示节点：其最后一帧必须停在 15 分钟前。
	// 不能在主循环跑完后再补一条 now-900 —— 环形缓冲会丢弃时间回退的点
	// （Ring.Add 与 AddMetrics 都拒绝 TS 不大于上一条的样本），
	// 那条样本被静默丢弃后节点仍显示在线，r_offline 永远不触发。
	const offlineNode = "n08"
	offlineCutoff := now - 900

	for i, sp := range specs {
		// 显式 ID：离线模拟与服务探测都按 ID 引用节点，
		// 用位置推导会在插入/重排 specs 时把场景指向错误的机器。
		id := sp.id
		st.UpsertNode(&model.Node{
			ID: id, Name: sp.name, Provider: sp.provider, Region: sp.region,
			Group: sp.group, ExpireAt: sp.expire, Price: sp.price,
			Currency: sp.cur, Weight: i, Note: "", Token: "demo-" + id,
		})

		cores := 2
		if sp.baseCPU > 30 {
			cores = 4
		}
		host := model.HostInfo{
			Hostname: sp.name, OS: sp.os, Kernel: "6.1.0-53-amd64",
			Arch: "x86_64", CPUModel: "Intel Xeon / AMD EPYC", Cores: cores, Virt: "KVM",
		}

		// 全年分层演示数据：粗粒度覆盖 365 天，细粒度覆盖最近 7 天和 24 小时。
		// 这样 7d/30d/365d 曲线都能在 demo 中看到真实聚合结果。
		// 密度按「看得见的精度」分配：365 天档 6 小时、30 天档 15 分钟，
		// 近 7 天与 24 小时保持分钟级。旧区间的采样密度远超聚合精度，
		// 只会拖慢 -demo 启动（单节点 2.7 万点 → 1.5 万点）。
		segments := []struct{ from, to, step int64 }{
			{now - 365*86400, now - 30*86400, 6 * 3600},
			{now - 30*86400, now - 7*86400, 900},
			{now - 7*86400, now - 86400, 60},
			{now - 86400, now, 30},
		}
		var total uint64 = 26_000_000_000
		first := true
		// 固定开机时刻：Uptime 是「t 时刻的自开机时长」，必须随 t 单调不减。
		// 原实现用 `now-t + 13 天`，越早的样本算出的值越大（365 天前的点
		// 约 378 天、最新点只有 13 天），导出与历史曲线里看起来像每次采样都重启。
		bootAt := now - 365*86400 - 86400*13
		addSample := func(t int64) {
			phase := float64(t%86400) / 86400 * 2 * math.Pi
			// 日间高、夜间低的双峰曲线 + 抖动
			wave := 0.55 + 0.35*math.Sin(phase-math.Pi/2)
			noise := (rnd.Float64() - 0.5) * 8

			cpu := clampF(sp.baseCPU*wave*1.15+noise, 0.4, 99)
			mem := clampF(sp.baseMem+wave*9+noise*0.5, 5, 97)

			// 制造两个正在发生的告警场景（最近 1 小时持续越界）
			if sp.name == "武汉-独服" && t > now-3600 {
				cpu = clampF(89+noise*0.6, 0, 99)
			}
			if sp.name == "netcup-RS1000" && t > now-3600 {
				mem = clampF(91+noise*0.4, 0, 97)
			}

			diskUsed := uint64(float64(59*1024) * sp.disk / 100 * 1024 * 1024)
			total += uint64(rnd.Intn(900_000))

			h := model.HostInfo{}
			if first {
				h = host
				first = false
			}
			st.AddMetrics(id, h, model.Metrics{
				TS:       t,
				CPUPct:   round2(cpu),
				Load1:    round2(cpu / 100 * float64(cores) * 1.6),
				Load5:    round2(cpu / 100 * float64(cores) * 1.3),
				Load15:   round2(cpu / 100 * float64(cores) * 1.1),
				MemUsed:  uint64(float64(4*1024) * mem / 100 * 1024 * 1024),
				MemTotal: 4 * 1024 * 1024 * 1024,
				SwapUsed: 0, SwapTotal: 2 * 1024 * 1024 * 1024,
				DiskUsed:  diskUsed,
				DiskTotal: 59 * 1024 * 1024 * 1024,
				NetRx:     total,
				NetTx:     total / 3,
				TCPConn:   int(cpu*2.1) + rnd.Intn(40),
				Uptime:    uint64(t - bootAt),
				Procs:     120 + rnd.Intn(60),
			})
		}
		offline := id == offlineNode
		for _, seg := range segments {
			for t := seg.from; t < seg.to; t += seg.step {
				if offline && t > offlineCutoff {
					continue
				}
				addSample(t)
			}
		}
		if offline {
			addSample(offlineCutoff)
		} else {
			addSample(now)
		}
	}

	// 服务探测目标
	svcs := []model.Service{
		{ID: "s1", NodeID: "n01", Name: "主控面板", Type: "http", Target: "https://43.156.17.202:8008", Interval: 60, Enabled: true},
		{ID: "s2", NodeID: "n01", Name: "imotao.com", Type: "http", Target: "https://imotao.com", Interval: 60, Enabled: true},
		{ID: "s3", NodeID: "n02", Name: "kvm.cx", Type: "http", Target: "https://kvm.cx", Interval: 60, Enabled: true},
		{ID: "s4", NodeID: "n03", Name: "冷行日记", Type: "http", Target: "https://imotao.com/blog", Interval: 120, Enabled: true},
		{ID: "s5", NodeID: "n15", Name: "MySQL 3306", Type: "tcp", Target: "58.49.150.222:3306", Interval: 60, Enabled: true},
		{ID: "s6", NodeID: "n15", Name: "nginx 443", Type: "tcp", Target: "58.49.150.222:443", Interval: 60, Enabled: true},
		{ID: "s7", NodeID: "n06", Name: "CDN 回源", Type: "http", Target: "https://cdn.nat.tf", Interval: 120, Enabled: true},
	}
	for i := range svcs {
		st.UpsertService(&svcs[i])
	}

	// 24 小时探测历史
	cert := map[string]int{"s2": 42, "s3": 8, "s7": 15, "s4": 63}
	for _, sv := range svcs {
		cd := -1
		if v, ok := cert[sv.ID]; ok {
			cd = v
		}
		for t := now - 86400; t <= now; t += int64(sv.Interval) {
			ok := true
			lat := 30 + rnd.Intn(120)
			if sv.ID == "s5" && t > now-7200 {
				ok = false // 制造一个服务不可用场景
				lat = 0
			}
			if rnd.Float64() < 0.004 {
				ok = false
			}
			st.AddServiceResult(model.ServiceResult{
				ServiceID: sv.ID, TS: t, OK: ok, LatencyMs: lat, CertDays: cd,
			})
		}
	}

	// 演示用告警规则：Duration 设为 0 以便启动后立即触发（真实规则见 alert.DefaultRules）
	for _, r := range []*model.AlertRule{
		{ID: "r_offline", Name: "节点失联", Metric: "offline", Op: "gt", Threshold: 1, Duration: 0, Severity: "critical", Enabled: true},
		{ID: "r_cpu", Name: "CPU 持续高负载", Metric: "cpu", Op: "gt", Threshold: 85, Duration: 0, Severity: "warn", Enabled: true},
		{ID: "r_mem", Name: "内存使用率过高", Metric: "mem", Op: "gt", Threshold: 88, Duration: 0, Severity: "warn", Enabled: true},
		{ID: "r_disk", Name: "磁盘空间不足", Metric: "disk", Op: "gt", Threshold: 85, Duration: 0, Severity: "critical", Enabled: true},
	} {
		st.UpsertRule(r)
	}

	// 历史告警记录（用于展示已恢复的条目）。
	// 规则 ID 必须指向真实存在的规则，否则筛选与统计会出现孤儿引用。
	st.OpenAlert(&model.Alert{
		ID: "al_hist_1", RuleID: "r_offline", RuleName: "节点失联",
		// 严重度与 r_offline 的 critical 保持一致；时间跨度必须是 900 秒，
		// 才能与 Value=900 和「15 分钟」的文案对上。
		NodeID: "n08", NodeName: "racknerd-纽约", Severity: "critical",
		Value: 900, Message: "演示：节点失联 15 分钟后恢复",
		StartedAt: now - 8600, ResolvedAt: now - 7700,
	})
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
