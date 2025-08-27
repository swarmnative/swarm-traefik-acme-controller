ARG GO_VERSION=1.22
ARG TARGETOS=linux
ARG TARGETARCH

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates build-base && update-ca-certificates
ENV GOSUMDB=sum.golang.org
COPY go.mod ./
RUN set -eux; \
  # First pass: with sumdb ON
  for p in https://proxy.golang.org https://goproxy.cn https://goproxy.io; do \
    echo "trying GOPROXY=$p (sumdb on)"; \
    go env -w GOPROXY=$p,direct GOSUMDB=sum.golang.org; \
    if go mod download -x; then exit 0; fi; \
    echo "go mod download failed via $p (sumdb on)"; \
  done; \
  # Second pass: disable sumdb verification (some networks block sum.golang.org)
  echo "retry with GOSUMDB=off"; \
  for p in https://goproxy.cn https://goproxy.io https://proxy.golang.org; do \
    echo "trying GOPROXY=$p (sumdb off)"; \
    go env -w GOPROXY=$p,direct GOSUMDB=off; \
    if go mod download -x; then exit 0; fi; \
    echo "go mod download failed via $p (sumdb off)"; \
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

