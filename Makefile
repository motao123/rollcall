BINARY_SERVER := probe-server
BINARY_AGENT  := probe-agent

.PHONY: all server agent linux clean demo fmt vet

all: server agent

server:
	go build -o $(BINARY_SERVER) ./cmd/server

agent:
	go build -o $(BINARY_AGENT) ./cmd/agent

# 交叉编译到 Linux（静态链接，无 CGO 依赖）
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/probe-server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/probe-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o dist/probe-agent-linux-arm64 ./cmd/agent
	@ls -lh dist/

demo: server
	./$(BINARY_SERVER) -addr 127.0.0.1:9188 -data ./data -demo

fmt:
	gofmt -w -s cmd internal

vet:
	go vet ./...

clean:
	rm -f $(BINARY_SERVER) $(BINARY_AGENT)
	rm -rf dist data
