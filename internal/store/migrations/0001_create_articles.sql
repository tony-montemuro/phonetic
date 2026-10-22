-- Articles double as the pipeline job queue. Timestamps are Unix milliseconds
-- (UTC). Large artifacts (HTML, scripts, audio) live on disk under
-- <data_dir>/articles/<id>/, not here.
--
-- Conventions for the queue columns:
--   * A row is runnable when status is a non-terminal stage and
--     next_attempt_at <= now (set to created_at on insert, pushed out for
--     retry backoff), and it is not leased (locked_until NULL or <= now).
--   * A worker claims a row by setting locked_until (expiry) and, since
--     migration 0002, lease_id (token). Every write that completes or fails a
--     stage must compare-and-set on (id, lease_id, status) so a worker whose
--     lease expired and was reclaimed can't overwrite the new owner's
--     results. Do not CAS on locked_until: heartbeats change it.
CREATE TABLE articles (
    -- AUTOINCREMENT: ids are never reused. They key the feed GUID and the
    -- on-disk artifact directory.
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    url              TEXT    NOT NULL UNIQUE,  -- normalized
    status           TEXT    NOT NULL CHECK (status IN
                         ('queued', 'fetching', 'extracting', 'scripting', 'synthesizing', 'ready', 'failed')),
    failed_stage     TEXT    CHECK (failed_stage IN
                         ('fetching', 'extracting', 'scripting', 'synthesizing')),
    error            TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  INTEGER NOT NULL,
    locked_until     INTEGER,

    title            TEXT,
    author           TEXT,
    site_name        TEXT,
    published_at     INTEGER,
    word_count       INTEGER,
    image_count      INTEGER,
    image_importance INTEGER CHECK (image_importance BETWEEN 0 AND 3), -- 0 none, 1 decorative, 2 helpful, 3 essential
    image_notes      TEXT,

    audio_bytes      INTEGER,
    duration_ms      INTEGER,

    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    ready_at         INTEGER
) STRICT;

CREATE INDEX articles_status_next_attempt ON articles (status, next_attempt_at);
