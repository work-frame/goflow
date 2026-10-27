-- The API deletes expired sessions every hour (DELETE ... WHERE expires_at <= now()).
-- Without this index that statement reads the whole sessions table each time.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
