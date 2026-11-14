-- Job types (Phase 15, Task 63). Every job so far is a demo job: its title
-- picks what worker.Demo does. A webhook job POSTs a JSON body to an HTTPS
-- URL, both kept in payload. Demo jobs have no payload.

ALTER TABLE jobs
    ADD COLUMN type    TEXT NOT NULL DEFAULT 'demo' CHECK (type IN ('demo', 'webhook')),
    ADD COLUMN payload JSONB;

-- A webhook job always has a URL (a string) and a body; nothing else has a
-- payload. The size limit is a backstop: the API allows a 64 KiB body and a
-- 2 KiB URL, and jsonb's text form adds a little.
--
-- IS NOT DISTINCT FROM, not =: without a url key jsonb_typeof returns NULL,
-- "NULL = 'string'" is NULL, and a CHECK that evaluates to NULL passes. A
-- webhook job with no URL would be stored (FAILURES.md, case 57).
ALTER TABLE jobs ADD CONSTRAINT jobs_payload_matches_type CHECK (
    (type = 'webhook') = (payload IS NOT NULL)
    AND (payload IS NULL OR (jsonb_typeof(payload -> 'url') IS NOT DISTINCT FROM 'string' AND payload ? 'body'))
    AND (payload IS NULL OR octet_length(payload::text) <= 131072)
);
