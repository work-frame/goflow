-- Jobs submitted through the API or the CLI.
CREATE TABLE jobs (
    id         TEXT        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    title      TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    status     TEXT        NOT NULL CHECK (status IN ('pending', 'queued', 'processing', 'completed', 'failed')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (updated_at >= created_at)
);

-- GET /api/v1/jobs lists oldest first, with the ID as tie-breaker.
CREATE INDEX jobs_created_at_id_idx ON jobs (created_at, id);

-- GET /api/v1/jobs?status=... filters by status. Workers (Phase 6) will poll for queued jobs.
CREATE INDEX jobs_status_created_at_idx ON jobs (status, created_at);