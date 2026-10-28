# VibeCI harness and sandbox broker: one image, two commands
# ("vibeci daemon" and "vibeci sandboxd"). Build from the repository root:
#   docker build -f deploy/Dockerfile -t vibeci:latest .
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/vibeci ./cmd/vibeci

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache git openssh-client ca-certificates tini \
 && addgroup -S -g 10001 vibeci \
 && adduser -S -D -H -u 10001 -G vibeci -h /data/home -s /sbin/nologin vibeci \
 && mkdir -p /data /run/vibeci /etc/vibeci \
 && chown 10001:10001 /data
COPY --from=build /out/vibeci /usr/local/bin/vibeci
LABEL org.opencontainers.image.source="https://github.com/vibeci/vibeci" \
      org.opencontainers.image.description="VibeCI keeps forks merged with their upstreams" \
      org.opencontainers.image.licenses="MIT"
# The harness runs unprivileged. sandboxd overrides the user in compose
# because it must open the Docker socket.
USER 10001:10001
WORKDIR /data
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/vibeci"]
CMD ["daemon"]
