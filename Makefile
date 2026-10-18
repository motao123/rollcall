BINARY_SERVER := rollcall-server
BINARY_AGENT  := rollcall-agent

.PHONY: all server agent linux clean clean-data demo fmt vet

all: server agent

server:
	go build -o $(BINARY_SERVER) ./cmd/server

agent:
	go build -o $(BINARY_AGENT) ./cmd/agent

# 交叉编译到 Linux（静态链接，无 CGO 依赖）
linux:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/rollcall-server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/rollcall-server-linux-arm64 ./cmd/server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/rollcall-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/rollcall-agent-linux-arm64 ./cmd/agent
	@ls -lh dist/

demo: server
	./$(BINARY_SERVER) -addr 127.0.0.1:9188 -data ./data -demo

fmt:
	gofmt -w -s cmd internal

vet:
	go vet ./...

# clean 只清理构建产物；data/ 是运行时状态（含监控历史与令牌），不在此删除。
clean:
	rm -f $(BINARY_SERVER) $(BINARY_AGENT)
	rm -rf dist

# 显式清空运行时数据，仅在确认要丢弃监控历史与令牌时使用。
clean-data:
	rm -rf data
