ARG GO_VERSION=1.26.6
ARG GO_FEATURE_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS toolchain
FROM mcr.microsoft.com/devcontainers/go:2-${GO_FEATURE_VERSION}-bookworm@sha256:090a56c6c9c4e80a1573c18c1225eb851b8c86c199fc929b07ea2a67f7b4850f

COPY --from=toolchain /usr/local/go /usr/local/go

ARG GOPLS_VERSION=v0.23.0
ARG DELVE_VERSION=v1.27.0

RUN apt-get update \
    && export DEBIAN_FRONTEND=noninteractive \
    && apt-get install --yes --no-install-recommends \
        ca-certificates \
        curl \
        git \
        jq \
        less \
        make \
        openssh-client \
        postgresql-client \
        procps \
        ripgrep \
        shellcheck \
        zsh \
    && rm -rf /var/lib/apt/lists/*

USER vscode
RUN go install golang.org/x/tools/gopls@${GOPLS_VERSION} \
    && go install github.com/go-delve/delve/cmd/dlv@${DELVE_VERSION}
