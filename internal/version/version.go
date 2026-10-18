// Package version 保存构建版本号。
// 通过 -ldflags "-X github.com/motao/rollcall/internal/version.Version=v1.5.0" 注入。
package version

// Version 是构建版本；未注入时为 dev。
var Version = "dev"
