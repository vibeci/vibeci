# Base image for VibeCI sandboxes. The merge agent's shell and the
# clean-room verification run in containers from this image (read-only root
# filesystem, non-root, no capabilities, no network unless the profile names
# one). The agent needs git and ordinary shell tools; add your project's
# toolchain in a derived image (see go.Dockerfile).
#   docker build -f deploy/sandbox/base.Dockerfile -t vibeci-sandbox:base deploy/sandbox
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache \
      git bash coreutils findutils grep sed gawk diffutils patch file \
      jq ripgrep make build-base ca-certificates \
 && addgroup -S -g 10001 sandbox \
 && adduser -S -D -H -u 10001 -G sandbox -h /tmp/home -s /bin/sh sandbox
USER 10001:10001
WORKDIR /workspace
