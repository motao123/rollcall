//go:build race

package store

// raceEnabled 让高开销压力测试在 -race 下缩减迭代，避免插桩放大后超时。
const raceEnabled = true
