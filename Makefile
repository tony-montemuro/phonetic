# phonetic build/dev tasks. Keep compatible with GNU Make 3.81 (macOS default).

GO       ?= go
BIN_DIR  := bin
BINARY   := phonetic
PKG      := ./cmd/phonetic

LINUX_BINARY := $(BIN_DIR)/$(BINARY)-linux-amd64

# Local dev settings for `make run` only (deliberately not exported, so tests
# never inherit them). Override on the command line: make run PHONETIC_ADDR=...
PHONETIC_ADDR      ?= 127.0.0.1:8080
PHONETIC_DATA_DIR  ?= data
PHONETIC_LOG_LEVEL ?= debug

.PHONY: build build-linux run test test-short vet fmt fmt-check clean

build:
	$(GO) build -trimpath -o $(BIN_DIR)/$(BINARY) $(PKG)

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o $(LINUX_BINARY) $(PKG)

run: build
	PHONETIC_ADDR='$(PHONETIC_ADDR)' \
	PHONETIC_DATA_DIR='$(PHONETIC_DATA_DIR)' \
	PHONETIC_LOG_LEVEL='$(PHONETIC_LOG_LEVEL)' \
	./$(BIN_DIR)/$(BINARY)

test:
	$(GO) test ./...

test-short:
	$(GO) test -short ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

# Fails if any file needs formatting.
fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

clean:
	rm -rf $(BIN_DIR)
