ARG GO_VERSION=1.22
ARG TARGETOS=linux
ARG TARGETARCH

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates build-base && update-ca-certificates
ENV GOSUMDB=sum.golang.org
COPY go.mod ./
RUN set -e; \
  for p in https://proxy.golang.org https://goproxy.cn https://goproxy.io; do \
    echo "trying GOPROXY=$p"; \
    go env -w GOPROXY=$p,direct; \
    go mod download -x && exit 0 || echo "go mod download failed with $p"; \
  done; \
  echo "all proxies failed"; exit 1
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/${GITHUB_REPOSITORY}" \
      org.opencontainers.image.description="ACME controller (Go) that updates Docker Swarm secrets for Traefik" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/controller /controller
USER nonroot:nonroot
ENTRYPOINT ["/controller"]

