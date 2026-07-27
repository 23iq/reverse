# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build

ARG VERSION=v1alpha
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY Dockerfile assets.go ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X github.com/23iq/reverse/internal/buildinfo.Version=${VERSION}" \
    -o /out/reverse \
    ./cmd/reverse && \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/reverse-container-init \
    ./cmd/reverse-container-init

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 65532 reverse && \
    adduser -S -D -H -u 65532 -G reverse reverse
COPY --from=build /out/reverse /usr/local/bin/reverse
COPY --from=build /out/reverse-container-init /usr/local/bin/reverse-container-init

ENTRYPOINT ["/usr/local/bin/reverse-container-init"]
CMD ["--server", "--server-config", "/etc/reverse/server.json"]
