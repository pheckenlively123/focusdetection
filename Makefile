# cr3blur — Canon CR3 subject-focus detection (Go)
#
#   make            build the cr3blur binary
#   make check      fmt-check + vet + test (CI-style gate)
#   make test       run unit tests
#   make build      go build ./cmd/cr3blur -> ./cr3blur
#   make vet        go vet ./...
#   make fmt        gofmt -s -w .
#   make tidy       go mod tidy
#   make clean      remove build output

BIN     := cr3blur
CMD     := ./cmd/cr3blur
PKG     := ./...

GO      ?= go
GOFMT   ?= gofmt

.PHONY: all build test vet check fmt fmt-check tidy clean

all: build

build:
	$(GO) build -o $(BIN) $(CMD)

test:
	$(GO) test $(PKG)

vet:
	$(GO) vet $(PKG)

# Gate: formatting, vet, tests.
check: fmt-check vet test

fmt:
	$(GOFMT) -s -w .

fmt-check:
	@unformatted=$$($(GOFMT) -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt -s needed on:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BIN)
