-- Outbox claims (Phase 17, Task 72).
--
-- The relay used to lock its outbox rows (FOR UPDATE) and keep them locked
-- while it waited for Redis, so deleting a job or an account waited too.
-- Now it claims rows in one short statement and commits: claim_token says
-- which pass took them, claimed_until (PostgreSQL's clock) when the claim
-- runs out. It talks to Redis holding no lock, then deletes only the rows
-- that still carry its token. A relay that dies leaves its claim behind;
-- once claimed_until passes, another pass takes the rows and sends them
-- again (at least once, as before).
ALTER TABLE job_outbox
    ADD COLUMN claim_token   TEXT CHECK (char_length(claim_token) BETWEEN 1 AND 100),
    ADD COLUMN claimed_until TIMESTAMPTZ,
    -- A claim is a token and an expiry, or neither.
    ADD CONSTRAINT job_outbox_claim_whole CHECK ((claim_token IS NULL) = (claimed_until IS NULL));
