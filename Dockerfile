# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.6
ARG ALPINE_VERSION=3.24
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION}@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS build

ARG VERSION
ARG VCS_REF
ARG BUILD_DATE

RUN apk add --no-cache ca-certificates git

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download \
    && go mod verify

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -trimpath \
      -ldflags="-s -w -buildid= \
        -X github.com/ryancswallace/jobman-control/internal/buildinfo.Version=${VERSION} \
        -X github.com/ryancswallace/jobman-control/internal/buildinfo.Commit=${VCS_REF} \
        -X github.com/ryancswallace/jobman-control/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /out/jobman-control \
      .

FROM alpine:${ALPINE_VERSION}@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime

RUN apk add --no-cache ca-certificates tini tzdata \
    && addgroup -S -g 10001 jobman-control \
    && adduser -S -D -u 10001 -G jobman-control -h /var/lib/jobman-control jobman-control \
    && mkdir -p /etc/jobman-control /var/lib/jobman-control \
    && chown -R jobman-control:jobman-control /var/lib/jobman-control

ARG VERSION
ARG VCS_REF
ARG BUILD_DATE
LABEL org.opencontainers.image.title="jobman-control" \
      org.opencontainers.image.description="Shared control plane for Jobman" \
      org.opencontainers.image.url="https://github.com/ryancswallace/jobman-control" \
      org.opencontainers.image.source="https://github.com/ryancswallace/jobman-control" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.created="$BUILD_DATE" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build --chown=root:root /out/jobman-control /usr/local/bin/jobman-control
COPY --from=build --chown=root:root \
    /src/LICENSE \
    /src/THIRD_PARTY_NOTICES.md \
    /usr/share/licenses/jobman-control/

USER 10001:10001
WORKDIR /var/lib/jobman-control
EXPOSE 8080

STOPSIGNAL SIGTERM
ENTRYPOINT ["/sbin/tini", "--", "jobman-control"]
