//go:build !unix

package notify

import "os"

// checkPathOwner 在无法读取 Unix uid 的平台上跳过属主校验。
func checkPathOwner(string, os.FileInfo) error { return nil }
