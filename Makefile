BINARY_SERVER := rollcall-server
BINARY_AGENT  := rollcall-agent
BINARY_SIGN   := rollsign

# 版本号注入：优先使用环境变量 VERSION，否则取 git 描述。
# 用 ifeq 而不是 ?=：?= 是递归赋值，每次展开都会重新执行 git describe。
ifeq ($(origin VERSION),undefined)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
# 单引号包裹：tag 名允许包含空格与 $()，不加引号会被 shell 当成语法解析。
VERSION_FLAG := -X 'github.com/motao/rollcall/internal/version.Version=$(VERSION)'

# 交叉编译的统一参数（静态链接，无 CGO 依赖）
LDFLAGS_CROSS := -s -w $(VERSION_FLAG)
# $(1)=产物名  $(2)=命令目录  $(3)=架构
define cross_build
GOOS=linux GOARCH=$(3) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS_CROSS)" \
	-o dist/$(1)-linux-$(3) ./cmd/$(2)
endef

.PHONY: all server agent rollsign linux clean clean-data demo fmt vet test

all: server agent

server:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o $(BINARY_SERVER) ./cmd/server

agent:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o $(BINARY_AGENT) ./cmd/agent

rollsign:
	go build -trimpath -ldflags "$(VERSION_FLAG)" -o $(BINARY_SIGN) ./cmd/rollsign

# 交叉编译到 Linux。
# 先清空 dist：否则上一轮/其它目标的旧产物会混进发布包。
# 所有架构都由 cross_build 生成，避免手写多行时漏掉某个组合
# （此前 rollsign 只出了 amd64，arm64 节点上无法签发清单）。
linux:
	rm -rf dist
	mkdir -p dist
	$(call cross_build,rollcall-server,server,amd64)
	$(call cross_build,rollcall-server,server,arm64)
	$(call cross_build,rollcall-agent,agent,amd64)
	$(call cross_build,rollcall-agent,agent,arm64)
	$(call cross_build,rollsign,rollsign,amd64)
	$(call cross_build,rollsign,rollsign,arm64)
	@ls -lh dist/

# -demo 模式会忽略 -data 并改用临时目录，这里不再传 -data。
demo: server
	./$(BINARY_SERVER) -addr 127.0.0.1:9188 -demo

fmt:
	gofmt -w -s cmd internal

vet:
	go vet ./...

test:
	go test ./...

# clean 只清理构建产物；data/ 是运行时状态（含监控历史与令牌），不在此删除。
clean:
	rm -f $(BINARY_SERVER) $(BINARY_AGENT) $(BINARY_SIGN)
	rm -rf dist

# 显式清空运行时数据，仅在确认要丢弃监控历史与令牌时使用。
# 用绝对路径 + 二次确认，避免 `make -C` 或误触时删到别处的目录。
clean-data:
	@test "$(CONFIRM)" = "yes" || { echo "拒绝执行：请使用 make clean-data CONFIRM=yes"; exit 1; }
	@echo "将删除 $(CURDIR)/data（监控历史与令牌）"
	rm -rf "$(CURDIR)/data"
