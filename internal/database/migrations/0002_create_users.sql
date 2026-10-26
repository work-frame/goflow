-- Accounts. Emails are stored lowercased by the app; the CHECK makes sure of it,
-- so UNIQUE on the plain column is enough to stop Dan@x.com and dan@x.com both registering.
CREATE TABLE users (
    id            TEXT        PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    email         TEXT        NOT NULL UNIQUE CHECK (email = lower(email) AND char_length(email) BETWEEN 3 AND 254),
    -- bcrypt hashes start with $2a$, $2b$ or $2y$. This refuses a plain-text password.
    password_hash TEXT        NOT NULL CHECK (password_hash ~ '^\$2[aby]\$\d\d\$.{53}$'),
    created_at    TIMESTAMPTZ NOT NULL
);