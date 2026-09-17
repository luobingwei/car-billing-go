# ============ 构建阶段 ============
FROM golang:1.23-alpine AS builder
WORKDIR /src

# 先拷贝依赖清单，利用 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码与模板，编译为静态二进制
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/car-billing .

# ============ 运行阶段 ============
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/car-billing /app/car-billing
COPY templates /app/templates
COPY data /app/data
RUN chown -R app:app /app

USER app

# 端口 / 数据文件 / 订单数据库 / 模板目录 / 后台密码均可通过环境变量覆盖
ENV PORT=8080 \
    FIRMS_DATA=/app/data/firms.json \
    ORDERS_DB=/app/data/orders.db \
    TEMPLATES_DIR=/app/templates \
    ADMIN_PASSWORD=123456

# 数据持久化目录（挂载卷，防止容器重建丢失费率数据）
VOLUME ["/app/data"]

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/car-billing"]
