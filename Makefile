# The plugin is built with CGO_ENABLED=0 on purpose.
#
# zvec's Go binding has two backends: cgo, which compiles against the engine's
# C headers, and a runtime-FFI one that dlopen()s the same shared library
# instead. The FFI backend is what makes this plugin portable — it builds on any
# machine with a Go toolchain, no C compiler, no headers, no pkg-config — and it
# loads the engine at start-up instead. `make deps` fetches that library.
#
# This is why `go run .` fails with "zvec/c_api.h: No such file or directory":
# that takes the cgo path, which needs headers this project deliberately does
# not carry. Use `make run`, or `go run -tags purego .`

BINARY  := zvec-plugin
GO      ?= go
GOOS    ?= $(shell $(GO) env GOOS)
GOARCH  ?= $(shell $(GO) env GOARCH)
LIBDIR  := lib/$(GOOS)_$(GOARCH)

export CGO_ENABLED = 0

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download the zvec engine for this platform into ./lib
	./scripts/fetch-zvec-lib.sh

.PHONY: build
build: ## Build the plugin binary
	$(GO) build -o $(BINARY) .

.PHONY: run
run: deps build ## Fetch the engine if needed, build, and run the plugin
	./$(BINARY)

.PHONY: test
test: ## Run the tests that need no engine
	$(GO) test ./...

.PHONY: test-integration
test-integration: deps ## Run every test, including those against the real engine
	ZVEC_LIBRARY_PATH=$(CURDIR)/$(LIBDIR) $(GO) test -tags integration -count=1 ./...

.PHONY: vet
vet: ## Vet the code, including the integration-tagged files
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

.PHONY: fmt
fmt: ## Format the Go sources
	gofmt -w $(shell find . -name '*.go' -not -path './lib/*')

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: dist
dist: deps build ## Lay out a deployable directory in ./dist
	rm -rf dist
	mkdir -p dist/lib
	cp $(BINARY) dist/
	cp -r lib/$(GOOS)_$(GOARCH) dist/lib/
	cp .env.inflow.example dist/
	@echo
	@echo "dist/ holds the binary and the engine beside it."
	@echo "Copy .env.inflow.example to dist/.env.inflow, fill it in, and run ./$(BINARY)."

.PHONY: clean
clean: ## Remove build output (keeps ./lib)
	rm -rf $(BINARY) dist

.PHONY: clean-all
clean-all: clean ## Also remove the downloaded engine
	rm -rf lib
