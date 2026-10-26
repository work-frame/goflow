-- One row per login. The token itself is never stored, only its SHA-256 hash,
-- so a copy of this table can't be used to log in.
CREATE TABLE sessions (
    token_hash BYTEA       PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    -- Deleting a user ends all of their sessions.
    user_id    TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

-- "Log out everywhere" and account deletion find a user's sessions by user_id.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);