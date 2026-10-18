package main

import "testing"

func TestIsLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9080", true},
		{"127.0.0.2:9080", true},
		{"[::1]:9080", true},
		{"localhost:9080", true},
		// 全接口写法与具体网卡地址都必须视为对外暴露
		{":9080", false},
		{"0.0.0.0:9080", false},
		{"192.168.1.10:9080", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isLoopbackListen(c.addr); got != c.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
