-- Leases (Phase 12). A 'processing' job belongs to the worker holding its
-- lease: a random token, valid until lease_expires_at on the DATABASE's
-- clock. The worker renews it while the job runs. When it runs out, the job
-- may be taken back and run elsewhere; every result is recorded only with
-- the current token, so a worker that lost its lease can't overwrite what
-- the next one did. This replaces the startup reset of every 'processing'
-- job, which was only safe with one worker process.

ALTER TABLE jobs
    ADD COLUMN lease_token      TEXT CHECK (char_length(lease_token) BETWEEN 1 AND 100),
    ADD COLUMN lease_expires_at TIMESTAMPTZ;

-- Jobs left 'processing' by the last process without leases get one that
-- has already run out, so the first lease sweep takes them back.
UPDATE jobs SET version = version + 1, lease_token = 'before-leases', lease_expires_at = now()
WHERE status = 'processing';

-- Processing and holding a lease are the same thing: no processing job
-- without an owner, no lease left behind on a finished job.
ALTER TABLE jobs ADD CONSTRAINT jobs_processing_has_lease CHECK (
    (status = 'processing') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
    AND (lease_token IS NULL) = (lease_expires_at IS NULL)
);

-- The sweep looks for expired leases; only 'processing' rows have one.
CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at) WHERE status = 'processing';
