BINARY  := resonate
BIN_DIR := bin
CMD     := ./cmd/resonate

.PHONY: build
build:
	go build -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: install
install:
	go install $(CMD)

.PHONY: run
run: build
	./$(BIN_DIR)/$(BINARY) $(ARGS)

.PHONY: test
test:
	go test ./...

.PHONY: coverage
coverage:
	go test ./... -race -covermode=atomic -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docs
docs:
	go run ./cmd/gendocs

.PHONY: docs-check
docs-check: docs
	@if [ -n "$$(git status --porcelain -- docs/cli)" ]; then \
		echo "docs/cli is stale — run 'make docs' and commit the result"; \
		git status --porcelain -- docs/cli; \
		git diff -- docs/cli; \
		exit 1; \
	fi

.PHONY: godoc
godoc:
	@command -v pkgsite >/dev/null 2>&1 || go install golang.org/x/pkgsite/cmd/pkgsite@latest
	pkgsite -open .

.PHONY: check
check: fmt-check vet test

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)

.PHONY: help
help:
	@echo "Targets:"
	@echo "  build       build bin/$(BINARY)"
	@echo "  install     go install the CLI onto \$$GOBIN/\$$GOPATH/bin"
	@echo "  run         build and run, e.g. make run ARGS='hit https://example.com --duration 5s'"
	@echo "  test        go test ./..."
	@echo "  coverage    go test ./... -race with coverage; writes coverage.out and prints the total"
	@echo "  vet         go vet ./..."
	@echo "  fmt         gofmt -w ."
	@echo "  fmt-check   fail if any file needs gofmt"
	@echo "  tidy        go mod tidy"
	@echo "  check       fmt-check + vet + test"
	@echo "  docs        regenerate docs/cli from the cobra command tree"
	@echo "  docs-check  fail if docs/cli is out of sync with the code"
	@echo "  godoc       browse source doc comments locally (pkg.go.dev-style UI via pkgsite)"
	@echo "  clean       remove bin/"
