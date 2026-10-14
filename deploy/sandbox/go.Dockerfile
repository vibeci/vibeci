# Sandbox image for Go projects. Pair it with profiles that point the module
# and build caches at /cache (see sandboxd.example.jsonc), so dependencies
# fetched by the prefetch step are available to the offline sandboxes.
#   docker build -f deploy/sandbox/go.Dockerfile -t vibeci-sandbox:go deploy/sandbox
FROM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d
RUN apk add --no-cache \
      git bash coreutils findutils grep sed gawk diffutils patch file \
      jq ripgrep make build-base ca-certificates \
 && addgroup -S -g 10001 sandbox \
 && adduser -S -D -H -u 10001 -G sandbox -h /tmp/home -s /bin/sh sandbox
ENV GOTOOLCHAIN=local
USER 10001:10001
WORKDIR /workspace
