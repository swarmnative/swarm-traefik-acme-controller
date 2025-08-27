ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates && update-ca-certificates
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(apk --print-arch | sed 's/x86_64/amd64/;s/aarch64/arm64/') \
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

