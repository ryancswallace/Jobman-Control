SHELL := /bin/bash
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:

PAGER := cat
GIT_PAGER := cat
GH_PAGER := cat
SYSTEMD_PAGER := cat
export PAGER GIT_PAGER GH_PAGER SYSTEMD_PAGER

PROJECT := jobman-control
MODULE := github.com/ryancswallace/jobman-control
BIN_DIR := bin
DIST_DIR := dist
COVERAGE_FILE := coverage.txt
COVERAGE_MIN ?= 30
JOBMAN_DIR ?= ../jobman
CONTRACT_DESTINATION := contracts/jobman/v1alpha1
UPDATE_SCRIPTS := ./devel/updates

GO ?= go
unexport GOROOT
DOCKER ?= docker
DOCKER_PROGRESS ?= plain

GO_VERSION := $(shell tr -d '[:space:]' < go.version)
GOTOOLCHAIN := go$(GO_VERSION)
export GOTOOLCHAIN

GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION ?= v1.7.12
GORELEASER_VERSION ?= v2.17.0
SYFT_VERSION ?= v1.46.0
CSPELL_VERSION ?= 10.0.1

GOLANGCI_LINT ?= $(BIN_DIR)/golangci-lint
GOVULNCHECK ?= $(BIN_DIR)/govulncheck
ACTIONLINT ?= $(BIN_DIR)/actionlint
GORELEASER ?= $(BIN_DIR)/goreleaser
SYFT ?= $(BIN_DIR)/syft
SYFT_VERSION_FILE := $(BIN_DIR)/.syft-$(SYFT_VERSION)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf '%s' dev)
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf '%s' unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE ?= $(PROJECT):local
GO_BUILD_FLAGS ?= -trimpath -mod=readonly
GO_LDFLAGS ?= -s -w -buildid= \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.Date=$(BUILD_DATE)
FUZZ_PACKAGE ?= ./internal/contracts
FUZZ_TARGET ?= FuzzDecodeJobRequest
FUZZ_TIME ?= 10s

.PHONY: help
help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-22s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: all ci
all: check ## Run the complete local verification workflow.
ci: check ## Alias for the complete CI gate.

.PHONY: versions
versions: ## Show selected project and development-tool versions.
	@printf 'project:          %s\n' '$(VERSION)'
	@printf 'Go:               %s\n' '$(GO_VERSION)'
	@printf 'golangci-lint:    %s\n' '$(GOLANGCI_LINT_VERSION)'
	@printf 'govulncheck:      %s\n' '$(GOVULNCHECK_VERSION)'
	@printf 'actionlint:       %s\n' '$(ACTIONLINT_VERSION)'
	@printf 'GoReleaser:      %s\n' '$(GORELEASER_VERSION)'
	@printf 'Syft:             %s\n' '$(SYFT_VERSION)'
	@printf 'cspell:           %s\n' '$(CSPELL_VERSION)'

.PHONY: setup bootstrap
setup: bootstrap ## Install pinned tools and download modules.
bootstrap: go-version-check tools download

.PHONY: go-version-check
go-version-check: ## Verify that the exact pinned Go toolchain is active.
	@actual="$$( $(GO) env GOVERSION 2>/dev/null || true )"; \
	expected='go$(GO_VERSION)'; \
	if [[ "$$actual" != "$$expected" ]]; then \
		echo "Go toolchain $$expected is required; active toolchain is $${actual:-unavailable}." >&2; \
		exit 2; \
	fi

.PHONY: tools
tools: tool-golangci-lint tool-govulncheck tool-actionlint tool-goreleaser tool-syft ## Install pinned validation and release tools.

.PHONY: tool-golangci-lint
tool-golangci-lint:
	@set -eu; \
	if ! $(GOLANGCI_LINT) version 2>/dev/null \
		| grep -Fq 'version $(patsubst v%,%,$(GOLANGCI_LINT_VERSION))'; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(BIN_DIR)/"; \
		mkdir -p $(BIN_DIR); \
		GOBIN=$(abspath $(BIN_DIR)) $(GO) install \
			github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi

.PHONY: tool-govulncheck
tool-govulncheck:
	@set -eu; \
	if ! $(GOVULNCHECK) -version 2>/dev/null \
		| grep -Fq '$(GOVULNCHECK_VERSION)'; then \
		echo "Installing govulncheck $(GOVULNCHECK_VERSION) into $(BIN_DIR)/"; \
		mkdir -p $(BIN_DIR); \
		GOBIN=$(abspath $(BIN_DIR)) $(GO) install \
			golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
	fi

.PHONY: tool-actionlint
tool-actionlint:
	@set -eu; \
	if ! $(ACTIONLINT) -version 2>/dev/null \
		| grep -Fq '$(patsubst v%,%,$(ACTIONLINT_VERSION))'; then \
		echo "Installing actionlint $(ACTIONLINT_VERSION) into $(BIN_DIR)/"; \
		mkdir -p $(BIN_DIR); \
		GOBIN=$(abspath $(BIN_DIR)) $(GO) install \
			github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION); \
	fi

.PHONY: tool-goreleaser
tool-goreleaser:
	@set -eu; \
	if ! $(GORELEASER) --version 2>/dev/null \
		| grep -Fq '$(patsubst v%,%,$(GORELEASER_VERSION))'; then \
		echo "Installing GoReleaser $(GORELEASER_VERSION) into $(BIN_DIR)/"; \
		mkdir -p $(BIN_DIR); \
		GOBIN=$(abspath $(BIN_DIR)) $(GO) install \
			github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION); \
	fi

.PHONY: tool-syft
tool-syft:
	@set -eu; \
	if ! test -x '$(SYFT)' || ! test -f '$(SYFT_VERSION_FILE)'; then \
		echo "Installing Syft $(SYFT_VERSION) into $(BIN_DIR)/"; \
		mkdir -p $(BIN_DIR); \
		GOBIN=$(abspath $(BIN_DIR)) $(GO) install \
			github.com/anchore/syft/cmd/syft@$(SYFT_VERSION); \
		touch '$(SYFT_VERSION_FILE)'; \
	fi

.PHONY: download
download: ## Download and verify Go modules.
	$(GO) mod download
	$(GO) mod verify

.PHONY: tidy
tidy: ## Update go.mod and go.sum.
	$(GO) mod tidy

.PHONY: mod-check
mod-check: ## Verify module files are tidy and downloaded content is intact.
	$(GO) mod verify
	$(GO) mod tidy -diff

.PHONY: format fmt
format: tool-golangci-lint ## Format Go source.
	$(GOLANGCI_LINT) fmt
fmt: format

.PHONY: format-check
format-check: tool-golangci-lint ## Check Go formatting.
	$(GOLANGCI_LINT) fmt --diff

.PHONY: lint
lint: tool-golangci-lint ## Run static analysis against the Linux release target.
	GOOS=linux CGO_ENABLED=0 $(GOLANGCI_LINT) run ./...

.PHONY: vet
vet: ## Run go vet independently.
	$(GO) vet ./...

.PHONY: workflow-check
workflow-check: tool-actionlint ## Validate GitHub Actions workflows.
	$(ACTIONLINT) .github/workflows/*.yml

.PHONY: shellcheck
shellcheck: ## Statically analyze repository shell scripts.
	@if command -v shellcheck >/dev/null 2>&1; then \
		find devel -type f -name '*.sh' -print0 | xargs -0 shellcheck; \
	elif $(DOCKER) info >/dev/null 2>&1; then \
		$(DOCKER) run --rm -v '$(CURDIR):/src:ro' -w /src \
			koalaman/shellcheck-alpine:v0.11.0 \
			$$(find devel -type f -name '*.sh' -print); \
	else \
		echo 'shellcheck requires shellcheck or a running Docker daemon.' >&2; \
		exit 2; \
	fi

.PHONY: vulncheck
vulncheck: tool-govulncheck ## Check reachable Go code for known vulnerabilities.
	$(GOVULNCHECK) ./...

.PHONY: test unittest
test: ## Run race-enabled unit and optional PostgreSQL integration tests.
	$(GO) test -race -shuffle=on ./...
unittest: test

.PHONY: integration-test
integration-test: ## Require and run PostgreSQL integration tests.
	@test -n "$(JOBMAN_CONTROL_TEST_DATABASE_URL)" || \
		(echo 'JOBMAN_CONTROL_TEST_DATABASE_URL is required.' >&2; exit 2)
	$(GO) test -race -count=1 ./internal/store/postgres

.PHONY: coverage coverage-check
coverage: ## Write an atomic aggregate coverage profile.
	$(GO) test -race -shuffle=on -covermode=atomic -coverpkg=./... \
		-coverprofile=$(COVERAGE_FILE) ./...
coverage-check: coverage ## Enforce the aggregate coverage floor.
	$(GO) tool cover -func=$(COVERAGE_FILE) \
		| awk -v minimum='$(COVERAGE_MIN)' -f devel/check-coverage.awk

.PHONY: fuzz
fuzz: ## Fuzz one decoder for a bounded duration.
	$(GO) test -run '^$$' -fuzz '^$(FUZZ_TARGET)$$' \
		-fuzztime '$(FUZZ_TIME)' $(FUZZ_PACKAGE)

.PHONY: contracts-sync
contracts-sync: ## Refresh the protocol snapshot from JOBMAN_DIR.
	$(GO) run ./devel/contractsync \
		-source '$(JOBMAN_DIR)/protocol' \
		-destination '$(CONTRACT_DESTINATION)'

.PHONY: contracts-check
contracts-check: ## Verify the checked-in protocol checksum lock.
	$(GO) run ./devel/contractsync \
		-check -destination '$(CONTRACT_DESTINATION)'

.PHONY: contracts-source-check
contracts-source-check: ## Verify the snapshot exactly matches JOBMAN_DIR.
	$(GO) run ./devel/contractsync \
		-check -source '$(JOBMAN_DIR)/protocol' \
		-destination '$(CONTRACT_DESTINATION)'

.PHONY: docs-check
docs-check: ## Verify Markdown whitespace and relative links.
	@if git --no-pager grep -nI -E '[[:blank:]]+$$' -- '*.md'; then \
		echo 'Markdown files contain trailing whitespace.' >&2; \
		exit 1; \
	fi
	$(GO) run ./devel/docscheck -root .

.PHONY: spellcheck
spellcheck: ## Spell-check the repository with a pinned cspell version.
	@if command -v cspell >/dev/null 2>&1 \
		&& [[ "$$(cspell --version)" = '$(CSPELL_VERSION)' ]]; then \
		cspell lint --dot .; \
	elif command -v npx >/dev/null 2>&1; then \
		npx --yes cspell@$(CSPELL_VERSION) lint --dot .; \
	elif $(DOCKER) info >/dev/null 2>&1; then \
		$(DOCKER) build --progress=$(DOCKER_PROGRESS) \
			--file Dockerfile.cspell --build-arg CSPELL_VERSION=$(CSPELL_VERSION) \
			--output type=cacheonly .; \
	else \
		echo 'cspell requires cspell, npx, or a running Docker daemon.' >&2; \
		exit 2; \
	fi

.PHONY: docs
docs: docs-check spellcheck ## Validate authored documentation.

.PHONY: build
build: ## Build the service binary with release metadata.
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' \
		-o $(BIN_DIR)/$(PROJECT) .

.PHONY: install
install: ## Install the service with the active Go toolchain.
	$(GO) install $(GO_BUILD_FLAGS) -ldflags='$(GO_LDFLAGS)' .

.PHONY: run
run: build ## Build and run the service using environment configuration.
	$(BIN_DIR)/$(PROJECT)

.PHONY: cross-build
cross-build: ## Compile every supported release OS and architecture.
	@set -eu; \
	for target in linux/amd64 linux/arm64 linux/386 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 windows/386; do \
		goos=$${target%/*}; \
		goarch=$${target#*/}; \
		echo "building $$goos/$$goarch"; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) ./...; \
	done

.PHONY: docker-check
docker-check: ## Validate runtime and devcontainer Dockerfiles.
	$(DOCKER) build --progress=$(DOCKER_PROGRESS) --check .
	$(DOCKER) build --progress=$(DOCKER_PROGRESS) --check \
		--file .devcontainer/Dockerfile .devcontainer

.PHONY: docker-image
docker-image: ## Build the local non-root service image.
	$(DOCKER) build --progress=$(DOCKER_PROGRESS) \
		--build-arg VERSION='$(VERSION)' \
		--build-arg VCS_REF='$(COMMIT)' \
		--build-arg BUILD_DATE='$(BUILD_DATE)' \
		--tag $(IMAGE) .

.PHONY: docker-smoke
docker-smoke: docker-image ## Verify the runtime image identity and permissions.
	DOCKER='$(DOCKER)' ./devel/container-smoke.sh \
		'$(IMAGE)' '$(VERSION)' '$(COMMIT)'

.PHONY: docker-run
docker-run: docker-image ## Run the service image with caller-supplied flags.
	$(DOCKER) run --rm $(DOCKER_RUN_FLAGS) $(IMAGE)

.PHONY: release-metadata-check
release-metadata-check: ## Verify metadata for the latest reachable stable tag.
	./devel/check-release-metadata.sh

.PHONY: release-check
release-check: tool-goreleaser release-metadata-check ## Validate release configuration and metadata.
	$(GORELEASER) check

.PHONY: release-build
release-build: tool-goreleaser ## Compile every GoReleaser target.
	$(GORELEASER) build --snapshot --clean

.PHONY: artifact-check
artifact-check: ## Verify a complete release artifact set already in dist/.
	./devel/check-release.sh $(DIST_DIR)

.PHONY: snapshot
snapshot: tool-goreleaser tool-syft ## Build a complete local release snapshot without publishing.
	PATH='$(abspath $(BIN_DIR))':$$PATH \
		$(GORELEASER) release --snapshot --clean --parallelism 1 --skip=sign
	$(MAKE) --no-print-directory artifact-check

.PHONY: package-smoke
package-smoke: ## Install snapshot packages in pinned Linux containers.
	./devel/package-smoke.sh $(DIST_DIR)

.PHONY: quick-check
quick-check: go-version-check mod-check format-check lint vet contracts-check test docs-check build ## Run the fast local gate.

.PHONY: check
check: go-version-check mod-check format-check lint vet workflow-check shellcheck vulncheck contracts-check coverage-check docs cross-build docker-smoke release-check release-build build ## Run the complete local gate.

.PHONY: update
update: ## Run deterministic repository-maintenance scripts.
	@set -eu; \
	export GO_VERS='$(GO_VERSION)'; \
	for script in $(sort $(wildcard $(UPDATE_SCRIPTS)/*.sh)); do \
		echo "Running $$script"; \
		"$$script"; \
	done

.PHONY: update-all
update-all: update format ## Run maintenance and formatting.

.PHONY: clean
clean: ## Remove local build, coverage, and release output.
	$(RM) -r $(BIN_DIR) $(DIST_DIR)
	$(RM) $(COVERAGE_FILE)
