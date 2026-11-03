-- Retries (Phase 7). PostgreSQL holds the whole truth about a job's
-- attempts; Redis only schedules when a retry is delivered. If Redis loses
-- its delayed set, the reconciler finds overdue 'retrying' jobs from here.

-- Two new statuses: 'retrying' (failed, waiting for its next attempt) and
-- 'dead' (permanent error, or out of attempts). 'failed' stays allowed so
-- older rows remain valid; new failures become 'retrying' or 'dead'.
ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('pending', 'queued', 'processing', 'retrying', 'completed', 'failed', 'dead'));

ALTER TABLE jobs
    -- Incremented by every successful claim, so a job that crashes its
    -- worker uses up attempts too and can't loop forever.
    ADD COLUMN attempts        INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN max_attempts    INT         NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    -- When a 'retrying' job is due. Set only while retrying.
    ADD COLUMN next_attempt_at TIMESTAMPTZ,
    -- The last failure, for operators. Not shown to users yet (Task 31).
    ADD COLUMN last_error      TEXT        CHECK (char_length(last_error) <= 1000),
    ADD CONSTRAINT jobs_retrying_has_due_time CHECK (status <> 'retrying' OR next_attempt_at IS NOT NULL);

-- The reconciler looks for overdue retries; only 'retrying' rows matter.
CREATE INDEX jobs_retrying_due_idx ON jobs (next_attempt_at) WHERE status = 'retrying';
