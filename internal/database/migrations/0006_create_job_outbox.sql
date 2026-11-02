-- Transactional outbox: one row per job whose ID still has to reach the queue.
--
-- The API inserts the job and its outbox row in ONE transaction, so either
-- both exist or neither does. A relay reads the rows, pushes the IDs to
-- Redis, then deletes the rows and marks the jobs queued. If Redis is down,
-- the rows simply wait. Nothing about creating a job depends on Redis.
CREATE TABLE job_outbox (
    -- Delivery order. GENERATED ALWAYS means only PostgreSQL assigns it.
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Deleting a job before the relay sends it removes its row too.
    job_id     TEXT        NOT NULL UNIQUE REFERENCES jobs (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL
);

-- Jobs created before the queue existed were never queued, and nothing
-- would ever pick them up. Give every pending job an outbox row now,
-- oldest first, so the relay delivers them too.
INSERT INTO job_outbox (job_id, created_at)
SELECT id, created_at FROM jobs WHERE status = 'pending' ORDER BY created_at, id;
