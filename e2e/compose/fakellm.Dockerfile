# The e2e tests' fake model and alert sink. Build from the repository root:
#   docker build -f e2e/compose/fakellm.Dockerfile -t vibeci-e2e-fakellm .
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY go.mod ./
COPY e2e/fakellm ./e2e/fakellm
RUN CGO_ENABLED=0 go build -trimpath -o /out/fakellm ./e2e/fakellm/cmd/fakellm

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
COPY --from=build /out/fakellm /usr/local/bin/fakellm
ENTRYPOINT ["/usr/local/bin/fakellm"]
