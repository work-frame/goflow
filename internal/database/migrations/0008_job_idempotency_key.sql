-- Idempotent job creation (Task 31). A client that sends the same
-- Idempotency-Key twice (a retry after a timeout, a double click) gets the
-- first job back instead of a second one.
--
-- The key is scoped to its owner: two users may pick the same key without
-- seeing each other's jobs. NULL means the request had no key; NULLs never
-- collide in a unique index, so jobs without a key are unaffected.
ALTER TABLE jobs
    ADD COLUMN idempotency_key TEXT CHECK (char_length(idempotency_key) BETWEEN 1 AND 255);

-- The guarantee lives here, not in Go: two concurrent requests with the same
-- key both try to INSERT, PostgreSQL lets exactly one commit, and the other
-- gets a unique violation naming this index. A "SELECT first, INSERT if
-- missing" check in Go would let both through.
CREATE UNIQUE INDEX jobs_user_idempotency_key ON jobs (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
