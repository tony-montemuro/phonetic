# phonetic: Plan

Phonetic turns web articles into a private podcast. I send it an article URL from my phone. It fetches the page, pulls out the article text, rewrites it so it sounds natural when read aloud, turns that into speech, and publishes the result as an episode in a private RSS feed. I listen in a normal podcast app, mostly while driving.

It is a single-user system that runs on my own hardware and is reachable only by me.

## Goals

- Get articles in with one tap from iOS (Share Sheet → Shortcut).
- Pull the article out of any site's HTML without writing code for each site.
- Flag articles that rely on their images, so I know to go back and look at them.
- Natural-sounding audio, with a TTS provider I can swap out (local first, hosted later).
- Use an existing podcast app for playback, queueing and "listened" tracking. No custom frontend.
- Go, mostly standard library; SQLite for storage; little else to run.

## Non-goals (for now)

- Multiple users, sign-up, or anything public.
- Paywalled content.
- Pages built entirely by JavaScript (headless browser fetching is deferred).
- A custom mobile app or web UI.
- Claude / MCP integration.
- Summary or short-version mode. The design should leave room for it, but it is out of scope.

## Architecture

```
 iPhone                                  Arch PC (systemd: phonetic)
┌────────────────────┐                  ┌──────────────────────────────────────────────────┐
│ Safari → Share →   │  POST /api/      │  HTTP server ──▶ SQLite (articles)                │
│ "Send to phonetic" │────articles─────▶│      │  ▲               ▲                         │
│  (Shortcut)        │◀──── 202 ────────│      │  │               │ claim / update          │
└────────────────────┘                  │      │  │        ┌──────┴───────────────────────┐ │
                                        │      │  │        │ Pipeline worker(s)           │ │
┌────────────────────┐  GET /feed.xml   │      │  │        │ fetch → extract → script →   │ │
│ Podcast app        │─────────────────▶│      │  │        │ synthesize → ready           │ │
│ (Apple Podcasts /  │  GET /audio/N.mp3│      │  └────────│  writes files to data dir    │ │
│  Downcast)         │─────────────────▶│      │           └──┬─────────┬─────────┬───────┘ │
└────────────────────┘                  │      ▼              │         │         │         │
        ▲                               │  data/articles/N/   ▼         ▼         ▼         │
        │ Tailscale (private network)   │  raw.html …     Web sites  Claude API  TTS server │
        └───────────────────────────────│  audio.mp3                 (Haiku)   (Kokoro,    │
                                        │                                       Docker)     │
                                        └──────────────────────────────────────────────────┘
```

One Go binary runs the HTTP server and the background workers. Kokoro runs next to it as its own Docker container. `ffmpeg` is installed on the host.

## Tech decisions

| Area | Decision | Notes |
|---|---|---|
| Language | Go | Standard library first: `net/http`, `encoding/xml`, `database/sql`, `log/slog`, `os/exec`. |
| Database | SQLite via `modernc.org/sqlite` | Pure Go, no C compiler needed. WAL mode. |
| Fetch | `net/http` GET | Realistic User-Agent, timeout, size limit. Headless browser (`chromedp`) deferred. |
| Extraction | `codeberg.org/readeck/go-readability/v2` | Port of Firefox Reader View (actively maintained Readeck fork; the original go-shiori module is untagged and stale). We parse with `golang.org/x/net/html` ourselves and walk the cleaned tree into typed blocks. |
| LLM | Claude API, Haiku 5.5, behind an `LLM` interface | About $0.10 / $0.50 per million input/output tokens under 100k-token prompts, so fractions of a cent per article. Look up the exact model ID from the Models API. Official Go SDK (`github.com/anthropics/anthropic-sdk-go`) vs. raw `net/http`: decide in Phase 6. A `claude -p` adapter is possible later. |
| TTS | `TTS` interface; Kokoro first | Kokoro-FastAPI in Docker serves an OpenAI-compatible `/v1/audio/speech`, so one adapter also covers OpenAI and similar hosted services. Piper adapter (runs the CLI) later. |
| Audio | `ffmpeg` (external binary) | Joins chunk audio into a single MP3 and reads the duration. |
| Delivery | RSS 2.0 + iTunes podcast tags | Built from the DB on each request; conditional GET (`304`) via `http.ServeContent`. |
| Network | Private VPN: Tailscale or plain WireGuard | phonetic only knows `PHONETIC_ADDR` and `PHONETIC_PUBLIC_URL`; nothing in the code is Tailscale-specific. Tailscale Funnel only if the chosen podcast app has to fetch from its own servers. |
| Podcast app | Apple Podcasts first, Downcast as fallback | Both appear to refresh on the phone, so they should work over Tailscale. Overcast / Pocket Casts would need Funnel. |
| Hosting | Arch Linux side of my desktop PC | systemd service; turn off sleep. The server is offline when the PC is off or in Windows. That is acceptable. |
| Containers | Kokoro only | phonetic runs as a native binary under systemd: Go builds a single static binary, its only system dependency is ffmpeg, and Tailscale and systemd cover networking and supervision. Kokoro (Python/PyTorch/CUDA) runs in Docker. Revisit if more services appear. |
| Build/deploy | `Makefile` | Cross-compile on the Mac (`GOOS=linux GOARCH=amd64`, possible because there is no cgo), `scp` to Arch, restart via systemd. |

## Data model

A single `articles` table doubles as the job queue. Large outputs live on disk, not in SQLite.

The schema lives in `internal/store/migrations/` (0001, 0002, ...). Those files are authoritative; this is a summary:

- `id INTEGER PRIMARY KEY AUTOINCREMENT`: ids are never reused, because they key the feed GUID and the artifact directory.
- `url` (unique, normalized), `status` (CHECK on the state set), `failed_stage`, `error`, `attempts`.
- Queue: `next_attempt_at` (NOT NULL; set to `created_at` on insert), `locked_until` (lease expiry), `lease_id` (random lease token, migration 0002). Lease-guarded writes compare-and-set on `(id, lease_id, status)`; heartbeats move only `locked_until`.
- Metadata: `title`, `author`, `site_name`, `published_at`, `word_count`, `image_count`, `image_importance` (0–3, CHECK), `image_notes`.
- Output: `audio_bytes`, `duration_ms`.
- `created_at`, `updated_at`, `ready_at`.
- All timestamps are INTEGER Unix milliseconds (UTC). The table is `STRICT`. Index on `(status, next_attempt_at)`.
- Migrations are forward-only, tracked in `PRAGMA user_version`, each run in its own transaction.

Files on disk, one directory per article:

```
data/
  phonetic.db
  articles/<id>/
    raw.html          # fetch output (converted to UTF-8)
    fetch.json        # fetch metadata: final URL after redirects, content type, time
    extracted.json    # extraction: metadata, typed blocks (paragraph/heading/list_item/quote/caption/preformatted/code/table), images with context
    script.txt        # text for TTS (LLM output, or extracted text as a fallback)
    analysis.json     # LLM image analysis
    chunks/           # intermediate TTS audio (deleted when done)
    audio.mp3         # final episode
```

Keeping every stage's output on disk means any stage can be re-run, for example after improving the LLM prompt, without fetching the page again.

## Pipeline

```
queued → fetching → extracting → scripting → synthesizing → ready
                \________\___________\____________\
                                                   → failed (failed_stage, error, attempts)
```

- **Workers.** N goroutines (default 1–2) claim the oldest article that can run, using a transaction that sets `status` and `locked_until`. On startup, any article whose lease has expired is put back as runnable. TTS uses the GPU, so the synthesize stage runs one job at a time.
- **Stages are idempotent.** Each stage reads the previous stage's file and writes its own. A retry resumes at `failed_stage`.
- **Retries.** Exponential backoff, at most about 3 attempts per stage, then it stays `failed` until reprocessed by hand.
- **Fetch.** GET with timeout, size limit, redirects followed. Save `raw.html`. Fail on non-2xx or non-HTML content.
- **Extract.** Run readability on the HTML with the final URL. Save title, byline, site name, published date, text, and an image list (src, alt, caption, nearby text). Count images here.
- **Script (LLM).** Send the extracted text plus image context to Haiku and ask for structured output:
  - `script`: text written for listening. Opens with a spoken intro ("*Title*, by *Author*, from *Site*"). Handles tables, code, footnotes, captions and link clutter, and briefly describes important images.
  - `image_importance` (0–3) and `image_notes`.
  - Vision: start with captions and nearby text only. Send the actual image bytes for the top few images later, if text-only judgments turn out weak.
  - **Fallback:** if the LLM step keeps failing, or is turned off in config, use the extracted text plus a generated intro as `script.txt`. The pipeline must work without an LLM.
- **Synthesize.** Split `script.txt` at paragraph and sentence boundaries into chunks under the provider's limit. Synthesize each chunk, join them with `ffmpeg`, read the duration, write `audio.mp3`, delete `chunks/`.

### Interfaces

```go
type Fetcher interface {
    Fetch(ctx context.Context, url string) (FetchResult, error) // HTML bytes + final URL
}

type LLM interface {
    Script(ctx context.Context, in ScriptInput) (ScriptOutput, error)
}

type TTS interface {
    MaxChunkChars() int
    Synthesize(ctx context.Context, text string, w io.Writer) error // writes audio (mp3/wav)
}
```

Adapters planned: `fetch.HTTP`; `llm.Anthropic`, `llm.None` (fallback/passthrough); `tts.OpenAICompatible` (Kokoro, OpenAI), `tts.Say` (macOS `say`, dev fallback only if CPU Kokoro is too slow), `tts.Piper`.

## HTTP API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/articles` | Bearer token | Body `{"url": "..."}`. New URL → `202 {id, status}`. Known URL → `200` with the existing record. |
| `GET` | `/api/articles` | Bearer token | List articles with status (for debugging). |
| `GET` | `/api/articles/{id}` | Bearer token | One article's status and metadata. |
| `POST` | `/api/articles/{id}/reprocess` | Bearer token | Re-run from a given stage (default: the failed stage). |
| `GET` | `/feed.xml` | Basic auth | Podcast feed of `ready` articles, newest first. Conditional GET supported. |
| `GET` | `/audio/{file}` | Basic auth | Episode audio, e.g. `/audio/42.mp3`, via `http.ServeFile` (range requests supported). ServeMux wildcards must be a whole segment, so the handler strips and checks the `.mp3` suffix and parses the id. |
| `GET` | `/healthz` | none | Readiness: 200 if the DB answers within 2s, else 503. |

Go 1.22+ `http.ServeMux` method/path patterns handle routing, so no router library is needed.

### Feed contents

Channel: title "phonetic", description, artwork (a static image served by phonetic), `itunes:author`, `itunes:explicit=false`.

Per episode:
- `title` = article title
- `itunes:author` = author
- `link` = source URL
- `guid` = `phonetic-article-<id>` (stable)
- `pubDate` = `ready_at`
- `enclosure` = audio URL, length, `audio/mpeg`
- `itunes:duration`
- `description` with a source link, site, word count, and an image note when `image_importance >= 2` (e.g. "📷 3 essential charts. Worth viewing later.")

Retention: the feed shows articles from the last N days (configurable). A cleanup job deletes audio for older articles and keeps the DB rows and text.

## Security

- **Network boundary:** a private VPN. The server listens only on loopback behind `tailscale serve`, or on the VPN interface (Tailscale IP or WireGuard address).
- **API:** bearer token from config, checked in constant time.
- **Feed/audio:** HTTP basic auth (Apple Podcasts and Downcast prompt for it). Required if we ever turn on Funnel; still worth having without it.
- **Secrets** (API token, feed password, Anthropic key) come from an environment file loaded by systemd and are never committed.
- **Fetching:** block private and loopback IP ranges (so a submitted URL can't reach internal services), cap response size, and set timeouts.

## Configuration

Environment variables only (no flags). `make run` supplies dev defaults. Blank values fall back to defaults, all errors are reported together, and relative paths are made absolute. For example:

```
PHONETIC_ADDR=100.x.y.z:8080
PHONETIC_DATA_DIR=/var/lib/phonetic
PHONETIC_PUBLIC_URL=https://pc.tailnet-name.ts.net    # base for enclosure URLs
PHONETIC_API_TOKEN=...
PHONETIC_FEED_USER=... / PHONETIC_FEED_PASSWORD=...
PHONETIC_WORKERS=2                     # 1-16
PHONETIC_FETCH_BLOCKED=203.0.113.7     # extra networks never to fetch (e.g. the home WAN IP); always enforced
PHONETIC_FETCH_ALLOW_PRIVATE=false     # DEV/TEST ONLY: disables SSRF protection; refused unless PHONETIC_ADDR is loopback
PHONETIC_RETENTION_DAYS=30
PHONETIC_LLM=anthropic | none
ANTHROPIC_API_KEY=...
PHONETIC_LLM_MODEL=<haiku 5.5 id>
PHONETIC_TTS_URL=http://127.0.0.1:8880/v1   # any OpenAI-compatible speech API
PHONETIC_TTS_MODEL=kokoro / PHONETIC_TTS_VOICE=af_heart / PHONETIC_TTS_SPEED=1
PHONETIC_TTS_API_KEY=...               # hosted APIs only
```

## Development & deployment

The user develops on macOS (Apple Silicon, `darwin/arm64`). The service runs on Arch Linux (`linux/amd64`, desktop PC with an RTX 3070).

| | macOS (dev) | Arch (prod) |
|---|---|---|
| Go | ✅ go1.27.3. `go.mod` requires ≥ 1.27.3 (crypto security fix). With the default `GOTOOLCHAIN=auto` an older `go` downloads 1.27.3; with `GOTOOLCHAIN=local` it refuses to build | not needed (binary is cross-compiled). The Go version is baked into the binary at build time, so rebuild and redeploy after toolchain security bumps. Check with `go version -m /usr/local/bin/phonetic` |
| ffmpeg | ✅ installed (Homebrew) | `pacman -S ffmpeg` |
| Docker | ✅ Docker Desktop (Kokoro on CPU) | `docker` + `nvidia-container-toolkit` (Kokoro on GPU) |
| Tailscale | later | required (Phase 4) |
| sqlite3 CLI | ✅ | optional |

**Local dev loop.** Run `make run`: phonetic runs with `PHONETIC_DATA_DIR=./data`, `PHONETIC_LLM=none` until Phase 6, and the TTS pointed at a local Kokoro container (`make kokoro-up`, CPU image). Test with curl against `localhost`. Check the feed in a browser or with `curl -u`. If CPU Kokoro is too slow to iterate with, switch dev to `PHONETIC_TTS=say`.

**Deploy.** `make deploy`:
1. `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/phonetic-linux-amd64 ./cmd/phonetic`
2. `scp` the binary to the Arch host (`DEPLOY_HOST` variable, over SSH, later via Tailscale)
3. `ssh $DEPLOY_HOST sudo systemctl restart phonetic`

Arch layout: binary at `/usr/local/bin/phonetic`, data in `/var/lib/phonetic`, secrets in `/etc/phonetic/env` (mode 600, loaded via `EnvironmentFile=`), unit at `/etc/systemd/system/phonetic.service`, run by systemd's `DynamicUser` (data really lives in `/var/lib/private/phonetic`, so the `sqlite3` CLI and backups need sudo). Kokoro via `deploy/kokoro.compose.yml` (GPU image) with `restart: unless-stopped`.

**Makefile targets:** `build`, `build-linux`, `run`, `test`, `test-short`, `test-e2e`, `test-all`, `vet`, `fmt`, `fmt-check`, `kokoro-up` / `kokoro-down`, `deploy`, `clean`. Note: macOS ships GNU Make 3.81, so avoid 3.82+ features such as `.ONESHELL`.

## Testing strategy

Three layers. Each phase adds to all three where it makes sense.

| Layer | What | How it runs |
|---|---|---|
| **Unit** | Pure logic: URL normalization, state transitions, chunking, feed XML building, config parsing, auth checks. | `go test ./...` (default, fast, no external tools). |
| **Integration** | One package against real dependencies: the store against a real SQLite file in `t.TempDir()`; the fetcher against `httptest` servers (redirects, timeouts, huge bodies, wrong content type, private IPs); extract against saved HTML fixtures; the audio package against real `ffmpeg`; the TTS adapter against a fake OpenAI-compatible server. | Same `go test`; tests that need `ffmpeg` call `t.Skip` when it isn't installed. Slow ones are guarded by `testing.Short()`. |
| **E2E** | Build the real binary and start it with a temp data dir, pointed at a **fake article site** (serves fixtures), a **fake LLM**, and a **fake TTS** that returns a real tone generated by ffmpeg. Post a URL over HTTP, poll until `ready`, fetch `/feed.xml` with basic auth, validate its structure, download the enclosure, and check it with `ffprobe`. Also: kill the binary mid-job and check that it recovers on restart. | `make test-e2e` (`//go:build e2e` tag, under `e2e/`). |

**Test artifacts:**
- `internal/extract/testdata/articles/`: openly licensed real pages, each with a golden `expected.json` (see its README). `articles-local/` (gitignored) holds pages from sites I actually read. Regenerate with `go test ./internal/extract -run TestFixtures -update`. Diffs get reviewed like code.
- `internal/feed/testdata/feed.xml`: golden feed for a known set of episodes (`go test ./internal/feed -update`).
- `internal/testutil/` (or `fakes/`): reusable fakes: an article site server, an OpenAI-compatible TTS server, an LLM server/stub, and helpers to start the binary and poll status.
- `cmd/phonetic-extract`: runs fetch and extract on a URL and prints the result; `-save DIR` captures a fixture (fixture format in `internal/extract/extracttest`).

**Makefile:** `test` (unit + integration), `test-short` (unit only), `test-e2e`, `test-all`.

## Project layout

See `ARCHITECTURE.md` for the current package map. New packages planned for later phases: `tts`, `audio`, `llm`, `feed`, and `deploy/` (systemd unit, Kokoro compose files).

## Phases

Each phase ends with something that runs. The goal is to get end-to-end audio in my podcast app early (end of Phase 4), then make it better.

### Phase 0: Setup ✅
- macOS dev tools are ready (Go, ffmpeg, Docker, sqlite3). Arch setup is deferred to Phase 4.
- `go mod init`, project layout, config loading, `slog` logging, `.gitignore` (`bin/`, `data/`, env files).
- `Makefile` with `build`, `build-linux`, `run`, `test`, `vet`, `fmt`, `clean` (`deploy` and `kokoro-*` targets added in later phases).
- SQLite store with embedded migrations.
- **Done when:** `make run` starts phonetic, creates the DB, serves `/healthz`; `make build-linux` produces a linux/amd64 binary.

### Phase 1: Ingest and queue ✅
- `POST/GET /api/articles` with bearer auth, URL normalization, deduplication.
- Worker loop with leases, the state machine, retries and backoff, all running stub stages.
- **Done when:** curl-posting a URL moves it through stub stages to `ready`, and a crash mid-job recovers.
- Carried over from Phase 0:
  - Shutdown order is HTTP, then workers, then store. Workers need a root context and a WaitGroup/errgroup in `main`, and the serve-error path must stop them too.
  - Claims: runnable statuses, `next_attempt_at <= now`, no live lease, `ORDER BY id` (depth-first, so the oldest article finishes first), optional stage filter (for the one-at-a-time GPU stage). Lease compare-and-set on `lease_id`. Never hold a write transaction across network, LLM or TTS calls. Read-only transactions pass `ReadOnly: true`.
  - modernc's busy wait ignores context cancellation, so a blocked write can stall shutdown for up to `busy_timeout` (5s).
  - `server.New` takes a `Deps` struct; extend it rather than adding positional arguments. Add `http.MaxBytesReader` on JSON bodies.
  - E2E helpers take `testing.TB` per call; the harness has `restart`, `kill` and `waitForLog` ready for recovery tests.

- Phase 1 outcome / notes for later phases:
  - Pipeline: `internal/pipeline` Runner with worker **pools** (`Options.Pools`; each stage in exactly one pool), lease heartbeats, claim-time attempt counting, `Permanent` errors, panic recovery, and per-pool wake-ups (`Notify`, stage hand-offs).
  - Stages get the claimed `store.Article`. Metadata they produce (title, word count, image importance...) must be written through **lease-guarded** store methods (CAS on `store.LeaseOf(a)`), not plain UPDATEs. Write stage files to a temp path and rename, so a superseded worker can't leave half-written outputs.
  - A data-dir `flock` enforces single-instance operation. `ReleaseAllLeases` at startup relies on it.
  - Resubmitting a URL whose article failed returns 200 with `status: failed` (no requeue). Phase 7's reprocess endpoint handles retries; the Shortcut can show the returned status.
  - Migrations are a small hand-rolled runner (`internal/store/migrate.go`). If the project gets serious, swap in `pressly/goose` or `golang-migrate`; only `store.Open` calls it.

### Phase 2: Fetch and extract ✅
- HTTP fetcher with timeouts, size limit, and the private-IP block.
- Readability extraction to `extracted.json`; image counting.
- Real-page fixtures with golden extractions in `internal/extract/testdata/articles/` (openly licensed) and `articles-local/` (gitignored, sites I read).
- **Done when:** posted URLs end with good `extracted.json` files and metadata in the DB.

- Phase 2 outcome / notes for later phases:
  - Stages live in `internal/stages`; artifacts in `internal/artifact` (atomic, durable writes). Fetch writes `raw.html` (UTF-8) + `fetch.json` (final URL); extract writes `extracted.json` and lease-guarded metadata (`store.SaveExtracted`).
  - `extract.Article` is typed blocks (`paragraph`, `heading`, `list_item`, `quote`, `caption`, `preformatted`, `code`, `table`), with `Block.Source` holding originals (TeX) when the spoken text differs. `Article.SpokenText()` is what Phase 3 should synthesize (code/tables become placeholders).
  - Known extraction gaps left for the Phase 6 LLM: Wikipedia end-of-article heading stubs and bibliographies are spoken; MDN loses its intro (readability drops `.layout__header`); bylines can include dates.
  - Fetcher SSRF guard: public addresses only, minus this machine's networks (public IPv6 widened to /56) and `PHONETIC_FETCH_BLOCKED`. Remaining gap: router hairpin to the WAN IPv4 — set `PHONETIC_FETCH_BLOCKED` to it in production.
  - E2E has a fake article site (`e2e/site_test.go`); instances run with `PHONETIC_FETCH_ALLOW_PRIVATE=true` (loopback only).

### Phase 3: TTS and audio ✅
- Kokoro container on the Mac (CPU) via `make kokoro-up`; pick a voice. The GPU setup on Arch happens in Phase 4.
- `tts.OpenAICompatible` adapter; chunking; ffmpeg concat; duration.
- If CPU Kokoro is too slow for development: add the `tts.Say` adapter (macOS `say` → AIFF → ffmpeg) for dev only.
- Use the `llm.None` passthrough so the extracted text goes straight to audio.
- **Done when:** a posted URL becomes a listenable `audio.mp3`.

- Phase 3 outcome / notes for later phases:
  - CPU Kokoro (`kokoro-fastapi-cpu:v0.9.0`) runs at about 4x real time on the Mac, so no `say` adapter. Voice `af_heart`, speed 1.0 (open to change after listening). GPU compose file: `deploy/kokoro.compose.yml`.
  - `stages.Script` is the passthrough (no `llm` package yet): an intro ("Title. By Author. From Site.") plus `SpokenText()`. Phase 6 replaces it; it should keep writing `script.txt`. Titles with a site suffix but no `site_name` (e.g. go.dev's " - The Go Programming Language") keep the suffix.
  - `stages.Synthesize`: `audio.Chunk` (1500 bytes, paragraph → sentence → word) → one WAV per chunk in `chunks/` (cached by hash of fingerprint + text) → ffmpeg filter-graph concat with 0.35 s gaps, loudnorm to -16 LUFS, 64 kb/s mono MP3 at 24 kHz → `store.SaveAudio` (bytes, duration). Memory of the ffmpeg join grows with total audio length (~300 MB for 45 min).
  - The engine's 400/413/422 fail the article permanently; Kokoro also answers an unknown voice with 400, so a voice typo fails articles at once (with Kokoro's message). Down engine = transient; with the default 3 attempts and 30 s backoff, an article fails if Kokoro is down for more than a couple of minutes. Phase 4/7: consider a longer backoff for synthesis, and the reprocess endpoint.
  - The API exposes `duration_ms` and `audio_bytes`; Phase 4's feed needs both (`<enclosure length>`, `<itunes:duration>`).
  - ffmpeg/ffprobe are checked at startup; the Arch box needs the `ffmpeg` package.
  - E2E instances get a fake speech engine (`e2e/tts_test.go`) by default; they never reach a real Kokoro.

### Phase 4: Feed and delivery (MVP) — code done; host setup pending
- `/feed.xml` with iTunes tags and conditional GET; `/audio/{file}` (`<id>.mp3`); basic auth; channel artwork.
- Arch setup: ffmpeg, Docker + `nvidia-container-toolkit`, Kokoro GPU container, `/etc/phonetic/env`.
- systemd unit; `make deploy`; turn off sleep.
- Tailscale or WireGuard on the PC and iPhone (and the Mac, for deploying). Subscribe in Apple Podcasts by URL. Fall back to Downcast, or Funnel, if needed.
- **Done when:** a new article shows up as an episode on my phone and plays in the car.

- Phase 4 outcome / notes for later phases:
  - `/feed.xml` (`internal/feed.Render`, RSS 2.0 + iTunes tags, golden-tested) and `/audio/<id>.mp3` behind basic auth (`PHONETIC_FEED_USER`/`PHONETIC_FEED_PASSWORD`); `/artwork.png` (drawn in code, 1400 px) without auth. Feed: body-hash ETag + `no-cache`; audio: `ServeContent` (HEAD, ranges) with a strong ETag for `If-Range`.
  - Links come from `PHONETIC_PUBLIC_URL` (default `http://<PHONETIC_ADDR>`, with a startup warning when that's loopback).
  - The feed lists up to 500 ready articles with audio, newest `ready_at` first. Phase 7 retention: the cleanup job must clear `audio_bytes` (the feed and `/audio/` skip articles without it) or filter by date.
  - Deploy: `deploy/phonetic.service` (DynamicUser, hardened, `KillMode=mixed` so ffmpeg isn't SIGTERMed mid-stage, `Restart=on-failure` covers a VPN address not up at boot), `deploy/phonetic.env.example`, `deploy/README.md` (one-time host steps), `make deploy DEPLOY_HOST=user@host`.
  - Not done here (needs the host and phone): the Arch setup, subscribing, and the "plays in the car" check.

### Phase 5: Share Sheet Shortcut
- Build the iOS Shortcut: accepts URLs from the Share Sheet → POST with bearer token → shows a notification.
- **Done when:** sharing from Safari queues an article in one tap.

### Phase 6: LLM script and image analysis
- Decide between the official Go SDK and raw HTTP; look up the Haiku 5.5 model ID.
- `llm.Anthropic` with structured output: script plus image importance and notes.
- Prompt work against the extraction fixtures (`internal/extract/testdata/`); fall back to passthrough on failure.
- Image note in feed descriptions.
- **Done when:** episodes sound like narration rather than a page read aloud, and image-heavy articles are flagged.

### Phase 7: Hardening
- Reprocess endpoint; retention cleanup job. The reprocess store method must refuse leased rows (or clear `locked_until` and `lease_id` in the same UPDATE); otherwise the article waits for the old lease to expire. It must also be able to restart from an earlier stage than `failed_stage` (e.g. extract fails permanently when the fetch output is missing; only re-fetching fixes that).
- Better failure visibility (e.g. failed articles appear as a short "failed: <title>" episode, or a status endpoint I check).
- SQLite backups (a periodic `VACUUM INTO`).
- Fill gaps in test coverage; E2E crash-recovery and retry scenarios.

### Later / ideas
- Headless browser fetch for JavaScript-built pages.
- Summary mode (a second, shorter script per article, or a separate feed).
- Piper and hosted TTS adapters; a voice per site.
- Sending actual images to the LLM (vision) for better importance scores.
- `claude -p` LLM adapter, if allowed by Anthropic's terms.
- Claude MCP connector for managing the queue from Claude.
- Moving to dedicated homelab hardware without a GPU, using hosted TTS.

## Open questions

- Exact Haiku 5.5 model ID and structured-output shape (Phase 6).
- Anthropic Go SDK vs. raw `net/http` (Phase 6). The SDK is the conventional choice; raw HTTP keeps dependencies minimal.
- Kokoro voice and speed: `af_heart` at 1.0 for now; revisit after listening in the car.
- Does Apple Podcasts accept a basic-auth feed added by URL (and plain HTTP, for the WireGuard option), and refresh it reliably over the VPN (Phase 4)?
- Retention window (default 30 days).
