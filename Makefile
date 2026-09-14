BINARY := auditaz
PKG := ./cmd/auditaz

# Optional version stamping:
#   make build VERSION=1.0.0
# The upload endpoint is a hardcoded source constant (cmd/auditaz/main.go),
# not a build flag — edit it there and recompile to change it.
VERSION ?= dev
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test vet fmt fmt-check tidy clean

build: ## Compile the CLI binary into ./auditaz
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

install: ## Install the CLI as `auditaz` into GOPATH/bin
	go install -ldflags "$(LDFLAGS)" $(PKG)
	@bin="$$(go env GOBIN)"; [ -n "$$bin" ] || bin="$$(go env GOPATH)/bin"; \
	echo "installed $$bin/$(BINARY)"; \
	case ":$$PATH:" in \
	  *":$$bin:"*) ;; \
	  *) echo "note: $$bin is not on your PATH, so \`$(BINARY)\` won't resolve yet."; \
	     echo "      run it as $$bin/$(BINARY), or add the directory:"; \
	     echo "      echo 'export PATH=\"$$bin:\$$PATH\"' >> ~/.zshrc && source ~/.zshrc";; \
	esac

test: ## Run all tests (includes the read-only guardrail in awsx)
	go test ./...

vet: ## Static checks
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmt-check: ## Fail if anything is unformatted (for CI)
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

tidy: ## Sync go.mod/go.sum
	go mod tidy

clean: ## Remove the built binary
	rm -f $(BINARY)
