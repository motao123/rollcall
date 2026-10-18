//go:build !linux

package main

import "errors"

func reexec(string, []string) error {
	return errors.New("自更新仅支持 Linux")
}
