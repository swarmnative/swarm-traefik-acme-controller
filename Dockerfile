ARG GO_VERSION=1.24
ARG TARGETOS=linux
ARG TARGETARCH

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates build-base && update-ca-certificates
COPY go.mod ./
# Online deps with proxy fallback (no vendor)
RUN set -eux; \
  for p in https://proxy.golang.org https://goproxy.cn https://goproxy.io; do \
    echo "trying GOPROXY=$p (sumdb on)"; \
    go env -w GOPROXY=$p,direct GOSUMDB=sum.golang.org; \
    if go mod download -x; then break; fi; \
    echo "go mod download failed via $p (sumdb on)"; \
  done; \
  if ! go list -m all >/dev/null 2>&1; then \
    echo "retry with GOSUMDB=off"; \
    for p in https://goproxy.cn https://goproxy.io https://proxy.golang.org; do \
      echo "trying GOPROXY=$p (sumdb off)"; \
      go env -w GOPROXY=$p,direct GOSUMDB=off; \
      if go mod download -x; then break; fi; \
      echo "go mod download failed via $p (sumdb off)"; \
    done; \
  fi
COPY cmd ./cmd
RUN go list -m all && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -v -trimpath -ldflags "-s -w" -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/${GITHUB_REPOSITORY}" \
      org.opencontainers.image.description="ACME controller (Go) that updates Docker Swarm secrets for Traefik" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/controller /controller
USER nonroot:nonroot
ENTRYPOINT ["/controller"]

