# syntax=docker/dockerfile:1

# ============================ 构建阶段 ============================
# 只在构建期间存在，最终镜像里不会保留 Go 工具链（几百 MB）
FROM golang:1.27-alpine AS build

WORKDIR /src

# 先只拷贝依赖清单。这一层能被 Docker 缓存：
# 只要 go.mod 没变，后续改代码就不会触发重新下载依赖。
COPY go.mod ./
RUN go mod download

# 再拷贝源码并编译
COPY . .

# CGO_ENABLED=0 产出纯静态二进制，不依赖 glibc，
# 因此可以放进任何精简的 Linux 镜像里运行。
#   -trimpath            去掉构建机器的绝对路径（避免泄露本地目录结构）
#   -ldflags="-s -w"     去掉符号表和调试信息，显著减小体积
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/matching-engine ./cmd/api

# ============================ 运行阶段 ============================
FROM alpine:3.21

# ca-certificates：以后要访问外部 HTTPS 服务时必需
# adduser：创建无特权用户，避免容器内进程以 root 运行
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app

COPY --from=build /out/matching-engine /usr/local/bin/matching-engine

# 切换到非 root 用户。容器逃逸时，攻击者拿到的也只是这个无权限账号。
USER 10001

EXPOSE 8080

# 监听地址由 main.go 读取，部署平台可覆盖
ENV ADDR=:8080

ENTRYPOINT ["/usr/local/bin/matching-engine"]
