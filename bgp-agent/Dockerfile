# syntax=docker/dockerfile:1.7
# Build context is bgp-agent/ only: no repository credentials or RouterOS state.
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY *.go ./
# These tests execute for BUILDPLATFORM. Only compilation sets TARGETARCH.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go test ./...
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -mod=readonly -buildvcs=false -trimpath -ldflags='-s -w' -o /out/bgp-agent .

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chmod=755 /out/bgp-agent /usr/bin/bgp-agent
RUN mkdir -p /etc/bgp-agent /data
LABEL org.opencontainers.image.title="mosdns BGP validation agent" \
      org.opencontainers.image.version="bgp-validation-20261114.6" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/jeffok/mosdns"
ENV TZ=UTC
EXPOSE 53/udp 53/tcp 179/tcp 9080/tcp
VOLUME ["/data"]
ENTRYPOINT ["/usr/bin/bgp-agent"]
CMD ["-config", "/etc/bgp-agent/config.json"]
