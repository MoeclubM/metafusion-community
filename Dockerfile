FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# 服务与数据搬运工具都是这一份源码的产物：搬运工具（cmd/migrate）只在切流窗口用
# `docker compose run --rm community-migrate -direction forward` 调用一次，
# 单独放进 /app/community-migrate，常驻服务不感知它。
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/community-server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/community-migrate ./cmd/migrate

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /app/community-server /app/community-server
COPY --from=builder /app/community-migrate /app/community-migrate
EXPOSE 8083
CMD ["/app/community-server"]
