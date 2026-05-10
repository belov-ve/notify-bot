FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY app/go.mod .
COPY app/*.go .
RUN go mod tidy
RUN go build -tags goolm -ldflags="-s -w" -o notify-bot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/notify-bot .
EXPOSE 8040-8050
ENV LOG_LEVEL=INFO
CMD ["/app/notify-bot"]