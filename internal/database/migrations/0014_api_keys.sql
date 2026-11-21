-- API keys (Task 86).
--
-- A key lets another program create or read its owner's jobs without a
-- login. Like a session, only the SHA-256 of the secret is stored; the
-- secret is shown once. hint is its last 4 characters, so a person can
-- tell keys apart. A key is never deleted on its own: revoking sets
-- revoked_at, and the row stays as history, and so jobs can still say which
-- key made them. Rows go only with their account.
CREATE TABLE api_keys (
    id           TEXT        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    user_id      TEXT        NOT NULL REFERENCES users ON DELETE CASCADE,
    name         TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    key_hash     BYTEA       NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    hint         TEXT        NOT NULL CHECK (char_length(hint) = 4),
    -- One or two known scopes, none twice. A key with an empty list could
    -- do nothing, so it isn't allowed to exist. (A CHECK can't hold a
    -- subquery; with two known scopes, "not twice" is "the two differ".
    -- For one scope, scopes[2] is NULL and the comparison passes.)
    scopes       TEXT[]      NOT NULL CHECK (
                     cardinality(scopes) BETWEEN 1 AND 2
                     AND scopes <@ ARRAY['jobs:create', 'jobs:read']::TEXT[]
                     AND scopes[1] IS DISTINCT FROM scopes[2]),
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL CHECK (expires_at > created_at),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- An account's keys, newest first (the list), and its active ones (the limit).
CREATE INDEX api_keys_user_idx ON api_keys (user_id, created_at DESC);

-- Which key created a job; NULL for jobs made by a logged-in person.
-- SET NULL, not CASCADE: the job and its history stay if the key's row
-- ever goes.
ALTER TABLE jobs ADD COLUMN api_key_id TEXT REFERENCES api_keys ON DELETE SET NULL;
CREATE INDEX jobs_api_key_idx ON jobs (api_key_id) WHERE api_key_id IS NOT NULL;
