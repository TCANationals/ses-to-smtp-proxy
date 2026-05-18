# ses-smtp-proxy - developer Makefile.
#
# Targets are deliberately thin wrappers around `go` and `makensis` so
# that contributors can run the same commands locally that CI runs.

# ----------------------------------------------------------------------
# Configuration
# ----------------------------------------------------------------------

PKG          := ./...
CMD_PKG      := ./cmd/ses-smtp-proxy
BIN_NAME     := ses-smtp-proxy
BIN_DIR      := bin
DIST_DIR     := dist
VERSION      ?= dev
CONFIG       ?= config.json
LDFLAGS      := -s -w -X main.version=$(VERSION)

# Cross-compile defaults; override on the command line for one-offs:
#   make build-windows ARCH=arm64
ARCH         ?= amd64

# ----------------------------------------------------------------------
# Phony declarations
# ----------------------------------------------------------------------

.PHONY: help all build debug run test test-race vet staticcheck \
        check fmt tidy clean cross build-windows installer \
        cfn-lint

.DEFAULT_GOAL := help

# ----------------------------------------------------------------------
# Help
# ----------------------------------------------------------------------

help: ## Show this help.
	@printf "ses-smtp-proxy - common developer targets\n\n"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ----------------------------------------------------------------------
# Build
# ----------------------------------------------------------------------

build: ## Build the service binary for the host platform into ./bin.
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BIN_NAME) $(CMD_PKG)

cross: build-windows ## Alias for cross-compile to the default Windows arch.

build-windows: ## Cross-compile windows/$(ARCH) into ./dist.
	@mkdir -p $(DIST_DIR)
	GOOS=windows GOARCH=$(ARCH) CGO_ENABLED=0 \
		go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST_DIR)/$(BIN_NAME)-$(ARCH).exe $(CMD_PKG)

installer: build-windows ## Build the NSIS installer for windows/$(ARCH) (requires makensis).
	@mkdir -p $(DIST_DIR)
	makensis -V2 \
		-DVERSION=$(VERSION) \
		-DARCH=$(ARCH) \
		-DEXE_PATH=../$(DIST_DIR)/$(BIN_NAME)-$(ARCH).exe \
		-DOUT_FILE=../$(DIST_DIR)/$(BIN_NAME)-$(VERSION)-$(ARCH)-setup.exe \
		installer/installer.nsi

# ----------------------------------------------------------------------
# Run
# ----------------------------------------------------------------------

debug: ## Run the service in the foreground with logs on stderr. CONFIG=path/to/config.json to override.
	go run $(CMD_PKG) debug --config $(CONFIG)

run: debug ## Alias for `make debug`.

# ----------------------------------------------------------------------
# Tests and checks
# ----------------------------------------------------------------------

test: ## Run unit tests.
	go test $(PKG) -count=1

test-race: ## Run unit tests with the race detector.
	go test $(PKG) -race -count=1

vet: ## go vet across the module.
	go vet $(PKG)

staticcheck: ## Run staticcheck (installs it on demand into GOBIN).
	@if ! command -v staticcheck >/dev/null 2>&1; then \
		echo "installing staticcheck..."; \
		go install honnef.co/go/tools/cmd/staticcheck@latest; \
	fi
	staticcheck $(PKG)

cfn-lint: ## Validate the CloudFormation template (requires cfn-lint on PATH).
	cfn-lint cloudformation/template.yaml

check: vet staticcheck test-race ## Run vet, staticcheck and tests with the race detector.

# ----------------------------------------------------------------------
# Housekeeping
# ----------------------------------------------------------------------

fmt: ## Format Go sources.
	gofmt -s -w $$(go list -f '{{.Dir}}' ./...)

tidy: ## Tidy go.mod / go.sum.
	go mod tidy

clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR)
