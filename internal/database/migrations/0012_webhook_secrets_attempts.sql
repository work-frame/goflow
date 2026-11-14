-- Webhook signing secrets and delivery attempts (Phase 15, Task 65b).
--
-- One signing secret per user, encrypted with GOFLOW_SECRETS_KEY
-- (internal/secrets): key_id names the key, nonce and ciphertext are
-- AES-256-GCM bound to the user's ID. A rotation keeps the previous secret
-- in prev_*, and deliveries sign with it too until rotated_at + 24 hours.

CREATE TABLE webhook_secrets (
    user_id         TEXT        PRIMARY KEY REFERENCES users ON DELETE CASCADE,
    key_id          TEXT        NOT NULL CHECK (key_id ~ '^[0-9a-f]{8}$'),
    nonce           BYTEA       NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext      BYTEA       NOT NULL,
    prev_key_id     TEXT        CHECK (prev_key_id ~ '^[0-9a-f]{8}$'),
    prev_nonce      BYTEA       CHECK (octet_length(prev_nonce) = 12),
    prev_ciphertext BYTEA,
    rotated_at      TIMESTAMPTZ NOT NULL,
    -- The previous secret is all there or not at all.
    CHECK ((prev_key_id IS NULL) = (prev_nonce IS NULL) AND (prev_nonce IS NULL) = (prev_ciphertext IS NULL))
);

-- Re-encryption after a key rotation looks for rows under other keys.
CREATE INDEX webhook_secrets_key_idx ON webhook_secrets (key_id);

-- One row per delivery attempt. A re-run starts attempts at 1 again, so
-- (job_id, attempt) isn't unique; the history keeps both runs.
CREATE TABLE webhook_attempts (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id      TEXT        NOT NULL REFERENCES jobs ON DELETE CASCADE,
    attempt     INT         NOT NULL CHECK (attempt >= 1),
    started_at  TIMESTAMPTZ NOT NULL,
    duration_ms INT         NOT NULL CHECK (duration_ms >= 0),
    status_code INT         CHECK (status_code BETWEEN 100 AND 599),
    outcome     TEXT        NOT NULL CHECK (outcome IN ('delivered', 'retry', 'permanent')),
    error       TEXT        CHECK (char_length(error) <= 300)
);

CREATE INDEX webhook_attempts_job_idx ON webhook_attempts (job_id, id);
