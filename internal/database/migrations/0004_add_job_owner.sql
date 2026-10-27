-- Every job now belongs to a user.
--
-- DATA DELETION: jobs created before accounts existed have no owner, and no
-- real user can claim them, so this migration deletes ALL existing jobs.
-- They were development test data. In a fresh database this deletes nothing.
-- The migration runs in one transaction: if any statement below fails, the
-- deletion is rolled back with it and nothing changes.
DELETE FROM jobs;

-- Deleting a user deletes their jobs.
ALTER TABLE jobs ADD COLUMN user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE;

-- Every list is now "this user's jobs, oldest first", so the index leads with
-- user_id. The old (created_at, id) index served lists across all users only.
DROP INDEX jobs_created_at_id_idx;
CREATE INDEX jobs_user_id_created_at_id_idx ON jobs (user_id, created_at, id);

-- jobs_status_created_at_idx stays: workers (Phase 6) will poll queued jobs
-- across all users.
