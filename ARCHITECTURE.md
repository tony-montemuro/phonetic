# phonetic architecture

How the code that exists today fits together. `PLAN.md` is the roadmap and
the reasoning behind decisions; this file is the map. Keep it short: it
points into the code rather than repeating it.

## The pipeline

One process. Each article is a row in the `articles` table and moves
through stages; each stage reads the previous stage's files and writes its
own under `data/articles/<id>/`.

```
POST /api/articles ─► queued
                        │
   fetching      internal/stages.Fetch    → raw.html, fetch.json
   extracting    internal/stages.Extract  → extracted.json + title/author/counts in the DB
   scripting     stub (Phase 6: LLM rewrite for listening)
   synthesizing  stub (Phase 3: TTS)
                        │
                      ready ─► (Phase 4: feed)
```

A failure becomes either a retry (transient) or `failed` with `failed_stage`
(permanent). See "Errors" below.

## Packages

| Package | Role | Start reading at |
|---|---|---|
| `cmd/phonetic` | Wiring: config, data-dir lock, store, pipeline, HTTP server, ordered shutdown | `run` |
| `cmd/phonetic-extract` | Dev tool: fetch + extract one URL; capture test fixtures | `run` |
| `internal/config` | Environment variables → `Config` (all errors reported together) | `Load` |
| `internal/server` | HTTP API: `/healthz`, `/api/articles` (bearer token) | `New` |
| `internal/urlnorm` | Canonical URL for deduplication | `Normalize` |
| `internal/store` | SQLite: articles table, the job queue and its leases, migrations | `ClaimArticle`, `leaseWrite` |
| `internal/pipeline` | Worker pools: claim → run stage (with heartbeat) → record outcome | `Runner.process` |
| `internal/stages` | The real stage implementations, plugged into the pipeline | `Fetch.Run`, `Extract.Run` |
| `internal/fetch` | Download a page safely (SSRF guard, limits, UTF-8) | `Fetcher.Fetch`, `defaultAllow` |
| `internal/extract` | Page → typed blocks + images + metadata (wraps go-readability) | `Extract`, `prepare`, `walker.walk` |
| `internal/artifact` | Atomic, durable per-article files | `Dir.writeAtomic` |

Dependency direction: `cmd` → `server`, `pipeline`, `stages` → `store`,
`fetch`, `extract`, `artifact`. The lower packages don't know about the
pipeline.

## Rules that cut across packages

**Leases.** A worker owns an article while it holds its lease
(`lease_id` + `locked_until`). Every write that records a stage's result is
a compare-and-set on `(id, lease_id, status)` (`store.leaseWrite`), so a
worker that lost its lease can't overwrite the new owner's result. A
heartbeat extends the lease while a stage runs. Attempts are counted at claim
time, so crash loops end. At startup, all leases are released; the data-dir
`flock` guarantees no other process holds them.

**Stages are re-runnable.** A stage may run again after a crash or retry, so
it must produce the same outcome: files are overwritten atomically
(`artifact`), metadata writes replace every field.

**Errors.** A stage returns `pipeline.Permanent(err)` when retrying can't
help (404, not HTML, no article content, missing input). Anything else is
retried with backoff, up to 3 attempts. Panics are recovered and retried.

**Fetching is untrusted.** URLs come from you, but pages can redirect
anywhere. `fetch` refuses non-public addresses and this machine's own
networks at connect time; `PHONETIC_FETCH_BLOCKED` adds more (set it to your
WAN IP in production). `PHONETIC_FETCH_ALLOW_PRIVATE` turns the built-in
rules off for local dev and E2E only.

**Shutdown order.** HTTP drains, then workers stop (interrupted stages
release their leases), then the store closes.

## Data model

`internal/store/migrations/` is authoritative. Timestamps are Unix
milliseconds; zero values in Go structs mean NULL. `extract.Article`
(`extracted.json`) is typed blocks; `Article.SpokenText()` is what gets read
aloud.

## Tests

- Unit and integration tests next to each package (`go test ./...`, `make test`).
- `internal/extract/testdata/articles/`: real pages with golden extractions
  (`go test ./internal/extract -run TestFixtures -update` to regenerate);
  `articles-local/` (gitignored) for pages from sites you read.
- `e2e/`: builds and runs the real binary against a fake article site
  (`make test-e2e`): crash recovery, shutdown, the data-dir lock, SSRF.
