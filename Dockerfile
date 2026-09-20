# ============ 运行阶段（预编译二进制，无需在 VPS 上 go build） ============
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY car-billing-linux /app/car-billing
COPY templates /app/templates
COPY data /app/data
RUN chmod +x /app/car-billing && chown -R app:app /app

USER app

ENV PORT=8080 \
    FIRMS_DATA=/app/data/firms.json \
    ORDERS_DB=/app/data/orders.db \
    TEMPLATES_DIR=/app/templates \
    ADMIN_PASSWORD=123456

VOLUME ["/app/data"]

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/car-billing"]
