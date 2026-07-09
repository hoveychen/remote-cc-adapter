# syntax=docker/dockerfile:1
#
# Relay image for muvee (or any HTTP-only PaaS). Builds the rca binary and runs
# `rca relay`, which listens for libp2p over WebSocket on :8080 so it can sit
# behind an HTTPS reverse proxy that terminates TLS. Only the relay role is used
# here — no native interceptor is needed, so a plain `go build` is enough.
#
# Deploy notes (muvee):
#   * container port 8080 (Traefik terminates TLS and forwards here)
#   * set RCA_RELAY_ANNOUNCE=/dns4/<prefix>.<base-domain>/tcp/443/tls/ws
#   * bind a stable RCA_RELAY_KEY secret so the relay PeerID survives redeploys
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w" -o /out/rca ./cmd/rca

FROM alpine:3.20
RUN adduser -D -u 10001 rca
COPY --from=build /out/rca /usr/local/bin/rca
USER rca
EXPOSE 8080
ENTRYPOINT ["rca", "relay"]
