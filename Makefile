BINARY_SERVER := rollcall-server
BINARY_AGENT  := rollcall-agent
BINARY_SIGN   := rollsign

# 版本号注入：优先使用环境变量 VERSION，否则取 git 描述。
# 用 ifeq 而不是 ?=：?= 是递归赋值，每次展开都会重新执行 git describe。
ifeq ($(origin VERSION),undefined)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
# 显式传入空值（CI 里 VERSION: "" 之类）时兜底为 dev：
# 否则 -X 会写成 Version=，二进制版本号变成空字符串，
# /metrics 的 build_info 与 -version 输出都会缺一块。
ifeq ($(strip $(VERSION)),)
VERSION := dev
endif
# VERSION_FLAG 最终位于 recipe 的双引号 shell 字符串里，而双引号内的
# 单引号只是普通字符，$ / ` 仍会触发命令替换。这些字符在版本号里没有
# 任何用途，直接拒绝，而不是指望引号保护。
# ★ 必须校验**未展开**的原始文本：命令行/环境传入的 VERSION 是递归变量，
# 写成 $(VERSION) 会先展开再检查，$(shell …) 这类值在检查前就已经执行了。
RAW_VERSION := $(value VERSION)
ifneq ($(findstring $$,$(RAW_VERSION)),)
$(error VERSION 不能包含 $: $(RAW_VERSION))
endif
ifneq ($(findstring `,$(RAW_VERSION)),)
$(error VERSION 不能包含反引号: $(RAW_VERSION))
endif
ifneq ($(findstring ",$(VERSION)),)
$(error VERSION 不能包含双引号: $(VERSION))
endif
ifneq ($(findstring ',$(VERSION)),)
$(error VERSION 不能包含单引号: $(VERSION))
endif
ifneq ($(findstring \,$(VERSION)),)
$(error VERSION 不能包含反斜杠: $(VERSION))
endif
# 单引号供链接器的 quoted.Split 使用：值里有空格时也不会被拆成多个参数。
VERSION_FLAG := -X 'github.com/motao123/rollcall/internal/version.Version=$(VERSION)'

# 交叉编译的统一参数（静态链接，无 CGO 依赖）
LDFLAGS_CROSS := -s -w $(VERSION_FLAG)
# $(1)=产物名  $(2)=命令目录  $(3)=架构
# -buildvcs=false：不把 git 状态/提交号嵌进二进制，
# 同一份源码与 VERSION 在任意机器上构建结果一致。
define cross_build
GOOS=linux GOARCH=$(3) CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS_CROSS)" \
	-o dist/$(1)-linux-$(3) ./cmd/$(2)
endef

# 发行签名私钥路径（仅 `make sign-release` 使用；私钥绝不能提交）
ROLLSIGN_KEY ?=

.PHONY: all server agent rollsign linux sign-release clean clean-data demo fmt vet test

all: server agent rollsign

server:
	go build -trimpath -buildvcs=false -ldflags "$(VERSION_FLAG)" -o $(BINARY_SERVER) ./cmd/server

agent:
	go build -trimpath -buildvcs=false -ldflags "$(VERSION_FLAG)" -o $(BINARY_AGENT) ./cmd/agent

rollsign:
	go build -trimpath -buildvcs=false -ldflags "$(VERSION_FLAG)" -o $(BINARY_SIGN) ./cmd/rollsign

# 交叉编译到 Linux。
# 先清空 dist：否则上一轮/其它目标的旧产物会混进发布包。
# 所有架构都由 cross_build 生成，避免手写多行时漏掉某个组合
# （此前 rollsign 只出了 amd64，arm64 节点上无法签发清单）。
# SHA256SUMS 必须逐个列出全部产物：写成 rollcall-* 会漏掉 rollsign-linux-*
# 与 rollcall-mcp-linux-*，而 rollsign 正是签发 Agent 更新清单的工具 ——
# 分发渠道被篡改时，最先该被校验和发现的恰恰是它。
linux:
	rm -rf dist
	mkdir -p dist
	$(call cross_build,rollcall-server,server,amd64)
	$(call cross_build,rollcall-server,server,arm64)
	$(call cross_build,rollcall-server,server,arm)
	$(call cross_build,rollcall-server,server,386)
	$(call cross_build,rollcall-agent,agent,amd64)
	$(call cross_build,rollcall-agent,agent,arm64)
	$(call cross_build,rollcall-agent,agent,arm)
	$(call cross_build,rollcall-agent,agent,386)
	$(call cross_build,rollcall-mcp,mcp,amd64)
	$(call cross_build,rollcall-mcp,mcp,arm64)
	$(call cross_build,rollcall-mcp,mcp,arm)
	$(call cross_build,rollcall-mcp,mcp,386)
	$(call cross_build,rollsign,rollsign,amd64)
	$(call cross_build,rollsign,rollsign,arm64)
	$(call cross_build,rollsign,rollsign,arm)
	$(call cross_build,rollsign,rollsign,386)
	# 先写临时文件再原子替换：`> SHA256SUMS` 会在 sha256sum 报错（产物缺失）
	# 之前就把上一次的校验和截断，留下一个空文件却仍然继续往下走。
	@cd dist && sha256sum rollcall-server-linux-* rollcall-agent-linux-* rollcall-mcp-linux-* rollsign-linux-* > SHA256SUMS.tmp && mv SHA256SUMS.tmp SHA256SUMS
	@ls -lh dist/
	@cat dist/SHA256SUMS

# 为 dist/ 中的四个服务端二进制生成 Ed25519 发行签名（<文件>.sig）。
# 先执行 make linux VERSION=v1.14.0，再在签名机上：
#   make sign-release ROLLSIGN_KEY=/root/rollcall-release.key VERSION=v1.14.0
# 私钥只留在签名机；公钥安装到控制端 /etc/rollcall/release.pub。
# -version 必须与构建时的 VERSION 完全一致，否则健康检查会因版本对不上而回滚。
sign-release: rollsign
	@test -n "$(ROLLSIGN_KEY)" || { echo "用法: make sign-release ROLLSIGN_KEY=/路径/rollcall-release.key VERSION=v1.14.0"; exit 1; }
	@test -f "$(ROLLSIGN_KEY)" || { echo "私钥不存在: $(ROLLSIGN_KEY)"; exit 1; }
	@for arch in amd64 arm64 arm 386; do \
		./$(BINARY_SIGN) sign-release -key "$(ROLLSIGN_KEY)" -version "$(VERSION)" \
			-file dist/rollcall-server-linux-$$arch -out dist/rollcall-server-linux-$$arch.sig || exit 1; \
	done

# -demo 模式会忽略 -data 并改用临时目录，这里不再传 -data。
demo: server
	./$(BINARY_SERVER) -addr 127.0.0.1:9188 -demo

# 扫全仓而不是只扫 cmd/internal：根目录或 tools/ 下的 Go 文件
# 同样会进 CI 的格式检查，这里保持一致。
fmt:
	gofmt -w -s .

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
	# 只接受命令行赋值：环境里残留的 CONFIRM=yes 不应让一条误敲的命令删库。
	@test "$(origin CONFIRM)" = "command line" && test "$(CONFIRM)" = "yes" \
		|| { echo "拒绝执行：请使用 make clean-data CONFIRM=yes"; exit 1; }
	@echo "将删除 $(CURDIR)/data（监控历史与令牌）"
	@test -n "$(CURDIR)" && test -d "$(CURDIR)/data" && rm -rf "$(CURDIR)/data"
