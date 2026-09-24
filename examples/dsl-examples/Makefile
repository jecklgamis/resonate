BIN_DIR := bin
MODELS  := constant-vus ramping-vus constant-arrival-rate ramping-arrival-rate bounded-arrival-rate per-vu-iterations shared-iterations

.PHONY: build
build:
	go build -o $(BIN_DIR)/full-scenario ./full-scenario
	@for m in $(MODELS); do go build -o $(BIN_DIR)/$$m ./$$m || exit 1; done

.PHONY: run-full-scenario
run-full-scenario:
	go run ./full-scenario

.PHONY: run-constant-vus
run-constant-vus:
	go run ./constant-vus

.PHONY: run-ramping-vus
run-ramping-vus:
	go run ./ramping-vus

.PHONY: run-constant-arrival-rate
run-constant-arrival-rate:
	go run ./constant-arrival-rate

.PHONY: run-ramping-arrival-rate
run-ramping-arrival-rate:
	go run ./ramping-arrival-rate

.PHONY: run-bounded-arrival-rate
run-bounded-arrival-rate:
	go run ./bounded-arrival-rate

.PHONY: run-per-vu-iterations
run-per-vu-iterations:
	go run ./per-vu-iterations

.PHONY: run-shared-iterations
run-shared-iterations:
	go run ./shared-iterations

.PHONY: run-all-models
run-all-models:
	@for m in $(MODELS); do echo "=== $$m ==="; go run ./$$m; echo; done

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

.PHONY: check
check: fmt-check vet

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) full-scenario/report.html

.PHONY: help
help:
	@echo "Targets:"
	@echo "  build                       build bin/full-scenario + one binary per execution model"
	@echo "  run-full-scenario           go run ./full-scenario"
	@echo "  run-constant-vus            go run ./constant-vus"
	@echo "  run-ramping-vus             go run ./ramping-vus"
	@echo "  run-constant-arrival-rate   go run ./constant-arrival-rate"
	@echo "  run-ramping-arrival-rate    go run ./ramping-arrival-rate"
	@echo "  run-bounded-arrival-rate    go run ./bounded-arrival-rate"
	@echo "  run-per-vu-iterations       go run ./per-vu-iterations"
	@echo "  run-shared-iterations       go run ./shared-iterations"
	@echo "  run-all-models              run every execution-model demo in sequence"
	@echo "  vet                         go vet ./..."
	@echo "  fmt                         gofmt -w ."
	@echo "  fmt-check                   fail if any file needs gofmt"
	@echo "  tidy                        go mod tidy"
	@echo "  check                       fmt-check + vet"
	@echo "  clean                       remove bin/ and full-scenario/report.html"
