FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY app/go.mod .
COPY app/*.go .
RUN go mod tidy
RUN go build -tags goolm -ldflags="-s -w" -o notify-bot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl jq bind-tools iputils bash
WORKDIR /app
COPY --from=builder /app/notify-bot .
EXPOSE 8040-8050
ENV LOG_LEVEL=INFO

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:${HEALTH_CHECK_PORT:-8040}/health || exit 1

CMD ["/app/notify-bot"]