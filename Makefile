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
PHONETIC_API_TOKEN ?= dev-token-not-secret-0000
PHONETIC_WORKERS   ?= 2

.PHONY: build build-linux run kokoro-up kokoro-down test test-short test-e2e test-all vet fmt fmt-check tidy-check clean

build:
	$(GO) build -trimpath -o $(BIN_DIR)/$(BINARY) $(PKG)

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -o $(LINUX_BINARY) $(PKG)

# @: don't echo the recipe, which contains the token.
run: build
	@PHONETIC_ADDR='$(PHONETIC_ADDR)' \
	PHONETIC_DATA_DIR='$(PHONETIC_DATA_DIR)' \
	PHONETIC_LOG_LEVEL='$(PHONETIC_LOG_LEVEL)' \
	PHONETIC_API_TOKEN='$(PHONETIC_API_TOKEN)' \
	PHONETIC_WORKERS='$(PHONETIC_WORKERS)' \
	PHONETIC_FETCH_ALLOW_PRIVATE='$(PHONETIC_FETCH_ALLOW_PRIVATE)' \
	PHONETIC_FETCH_BLOCKED='$(PHONETIC_FETCH_BLOCKED)' \
	PHONETIC_TTS_URL='$(PHONETIC_TTS_URL)' \
	PHONETIC_TTS_MODEL='$(PHONETIC_TTS_MODEL)' \
	PHONETIC_TTS_VOICE='$(PHONETIC_TTS_VOICE)' \
	PHONETIC_TTS_SPEED='$(PHONETIC_TTS_SPEED)' \
	PHONETIC_TTS_API_KEY='$(PHONETIC_TTS_API_KEY)' \
	./$(BIN_DIR)/$(BINARY)

# Local Kokoro TTS (CPU) for `make run`; see deploy/ for the GPU version.
KOKORO_COMPOSE := docker compose -f deploy/kokoro.dev.compose.yml

kokoro-up:
	$(KOKORO_COMPOSE) up -d --wait

kokoro-down:
	$(KOKORO_COMPOSE) down

test:
	$(GO) test -race ./...

test-short:
	$(GO) test -short ./...

# Builds and runs the real binary (race-enabled); see e2e/.
test-e2e:
	$(GO) test -tags e2e -race -count=1 ./e2e/...

test-all: test test-e2e

vet:
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/...
	$(GO) vet -tags 'e2e race' ./e2e/...

fmt:
	gofmt -s -w .

# Fails if any file needs formatting.
fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

# Fails if go.mod/go.sum are not tidy.
tidy-check:
	$(GO) mod tidy -diff

clean:
	rm -rf $(BIN_DIR)
