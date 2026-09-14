FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/community-server ./cmd/server

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /app/community-server /app/community-server
EXPOSE 8083
CMD ["/app/community-server"]
