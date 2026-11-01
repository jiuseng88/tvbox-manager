# ---------- 构建阶段 ----------
# 版本需与 go.mod 的 go 指令一致（go.mod: go 1.27.0），否则 go build 会报「requires go >= 1.27.0」
FROM golang:1.27-alpine AS builder

WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath \
    GOPROXY=https://goproxy.cn,direct

# 先拷 go.mod 利用层缓存
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/tvbox-manager .

# ---------- 运行阶段 ----------
FROM alpine:3.20

# 换成国内源，避免 dl-cdn 慢导致构建卡住（该配置会固化进镜像）
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata wget git \
    && addgroup -S app && adduser -S app -G app \
    && mkdir -p /data && chown -R app:app /data

WORKDIR /app
COPY --from=builder /out/tvbox-manager .

# 这三项只是容器内的默认值，首次启动时写入 config.json；
# 之后以后台「设置」/ config.json 为准，环境变量不再覆盖（ENV_OVERRIDE=1 可强制覆盖）
ENV TZ=Asia/Shanghai \
    DATA_DIR=/data \
    PORT=8080 \
    SUB_PATH=/sub

USER app
EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

ENTRYPOINT ["./tvbox-manager"]
