FROM golang:1.24-alpine AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/kubetorrent-portal .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 portal
WORKDIR /app
COPY --from=builder /out/kubetorrent-portal /usr/local/bin/kubetorrent-portal
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kubetorrent-portal"]
