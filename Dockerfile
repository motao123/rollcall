# Rollcall 服务端镜像。
# 项目零第三方依赖（go.mod 只有 module 行，无 go.sum），
# 构建阶段不需要下载模块，缓存层也只需 go.mod 一行。
#
# 构建：  docker build --build-arg VERSION=v1.20.0 -t rollcall .
# 运行：  docker run -d -p 19080:19080 -v rollcall-data:/data rollcall
# TLS：   建议挂反向代理终结 TLS；直挂公网请挂载证书并追加
#         -tls-cert /tls/cert.pem -tls-key /tls/key.pem 并去掉 -allow-insecure
#
# 运行镜像用 distroless/static：自带 CA 证书（HTTPS 探测与通知渠道必需）
# 与 tzdata，无 shell、无包管理器，攻击面与镜像体积都最小；
# 二进制为 CGO_ENABLED=0 静态链接，与 systemd 部署的产物完全一致。

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X 'github.com/motao123/rollcall/internal/version.Version=${VERSION}'" \
    -o /out/rollcall-server ./cmd/server \
    && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rollcall-server /rollcall-server
# 预建 /data 并归 nonroot：新命名卷会继承镜像内目录的属主，
# 否则 nonroot 进程写不了数据目录；bind mount 仍需用户自行 chown。
COPY --from=build --chown=nonroot:nonroot /out/data /data
RUN ["/rollcall-server", "-version"]
USER nonroot:nonroot
VOLUME /data
EXPOSE 19080
ENTRYPOINT ["/rollcall-server"]
# 容器网络不在宿主回环上，明文监听必须显式声明；
# 生产请用 -tls-cert/-tls-key 或反代，并把下面这行的 -allow-insecure 去掉。
CMD ["-addr", "0.0.0.0:19080", "-data", "/data", "-allow-insecure"]
