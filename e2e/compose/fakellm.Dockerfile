# The e2e tests' fake model and alert sink. Build from the repository root:
#   docker build -f e2e/compose/fakellm.Dockerfile -t vibeci-e2e-fakellm .
FROM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
COPY go.mod ./
COPY e2e/fakellm ./e2e/fakellm
RUN CGO_ENABLED=0 go build -trimpath -o /out/fakellm ./e2e/fakellm/cmd/fakellm

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY --from=build /out/fakellm /usr/local/bin/fakellm
ENTRYPOINT ["/usr/local/bin/fakellm"]
