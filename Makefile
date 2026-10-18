BINARY_SERVER := rollcall-server
BINARY_AGENT  := rollcall-agent

# 版本号注入：优先使用环境变量 VERSION，否则取 git 描述
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_FLAG := -X github.com/motao/rollcall/internal/version.Version=$(VERSION)

.PHONY: all server agent rollsign linux clean clean-data demo fmt vet test

all: server agent

server:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o $(BINARY_SERVER) ./cmd/server

agent:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o $(BINARY_AGENT) ./cmd/agent

rollsign:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o rollsign ./cmd/rollsign

# 交叉编译到 Linux（静态链接，无 CGO 依赖）
linux:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $(VERSION_FLAG)" \
		-o dist/rollcall-server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $(VERSION_FLAG)" \
		-o dist/rollcall-server-linux-arm64 ./cmd/server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $(VERSION_FLAG)" \
		-o dist/rollcall-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $(VERSION_FLAG)" \
		-o dist/rollcall-agent-linux-arm64 ./cmd/agent
	@ls -lh dist/

demo: server
	./$(BINARY_SERVER) -addr 127.0.0.1:9188 -data ./data -demo

fmt:
	gofmt -w -s cmd internal

vet:
	go vet ./...

test:
	go test ./...

# clean 只清理构建产物；data/ 是运行时状态（含监控历史与令牌），不在此删除。
clean:
	rm -f $(BINARY_SERVER) $(BINARY_AGENT)
	rm -rf dist

# 显式清空运行时数据，仅在确认要丢弃监控历史与令牌时使用。
# 用绝对路径 + 二次确认，避免 `make -C` 或误触时删到别处的目录。
clean-data:
	@test "$(CONFIRM)" = "yes" || { echo "拒绝执行：请使用 make clean-data CONFIRM=yes"; exit 1; }
	@echo "将删除 $(CURDIR)/data（监控历史与令牌）"
	rm -rf "$(CURDIR)/data"
