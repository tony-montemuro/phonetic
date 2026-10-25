# phonetic architecture

The map of the code as it exists. `PLAN.md` holds the roadmap and the
reasoning behind decisions; this file points into the code rather than
repeating it.

## The pipeline

One process. Each article is a row in the `articles` table and moves
through stages; each stage reads the previous stage's files and writes its
own under `data/articles/<id>/`.

```
POST /api/articles ─► queued
                        │
   fetching      internal/stages.Fetch    → raw.html, fetch.json
   extracting    internal/stages.Extract  → extracted.json + title/author/counts in the DB
   scripting     stub
   synthesizing  stub
                        │
                      ready
```

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
| `internal/stages` | The real stage implementations | `Fetch.Run`, `Extract.Run` |
| `internal/fetch` | Download a page safely (SSRF guard, limits, UTF-8) | `Fetcher.Fetch`, `defaultAllow` |
| `internal/extract` | Page → typed blocks + images + metadata (wraps go-readability) | `Extract`, `prepare`, `walker.walk` |
| `internal/artifact` | Atomic, durable per-article files | `Dir.writeAtomic` |

Only `cmd` wires packages together. `stages` adapts `fetch`, `extract`,
`artifact` and `store` to `pipeline`'s `Stage` interface; those four import
nothing from the pipeline layer.

## Rules that cut across packages

**Leases.** A worker owns an article while it holds its lease (`lease_id` +
`locked_until`). Every *database* write that records a stage's result is a
compare-and-set on `(id, lease_id, status)` (`store.leaseWrite`), so a worker
that lost its lease can't overwrite the new owner's result. A heartbeat
extends the lease while a stage runs; attempts are counted at claim time, so
crash loops end. At startup all leases are released, which is safe because
the data-dir `flock` guarantees no other process holds them.

**Stages are re-runnable.** A stage may run again after a crash, a retry or
a lost lease, so it must produce the same outcome: files are overwritten
atomically (`artifact`) and metadata writes replace every field. Stage files
are *not* lease-guarded; re-runnability is what makes that safe.

**Errors.** A stage returns `pipeline.Permanent(err)` when retrying can't
help (404, not HTML, no article content, missing input). Anything else is
retried with backoff, up to `pipeline.Options.MaxAttempts` (default 3) per
stage. Panics are recovered and retried.

**Fetching is untrusted.** Pages can redirect anywhere. `fetch` refuses
non-public addresses and this machine's own networks at connect time, plus
any configured blocks (see `internal/config`).

**Shutdown order.** HTTP drains, then workers stop (interrupted stages
release their leases), then the store closes.

The schema lives in `internal/store/migrations/`; the testing layout is in
`PLAN.md` ("Testing strategy").
