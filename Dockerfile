# ---------- 构建阶段 ----------
FROM golang:1.23-alpine AS builder

WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath

# 先拷 go.mod 利用层缓存
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/tvbox-manager .

# ---------- 运行阶段 ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S app && adduser -S app -G app \
    && mkdir -p /data && chown -R app:app /data

WORKDIR /app
COPY --from=builder /out/tvbox-manager .

ENV TZ=Asia/Shanghai \
    DATA_DIR=/data \
    PORT=8080

USER app
EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["./tvbox-manager"]
