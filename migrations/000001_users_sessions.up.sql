-- Accounts with two fixed roles. The application stores the login already
-- normalized (trimmed and lowercased); uniqueness is enforced here.
CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    login         TEXT        NOT NULL UNIQUE CHECK (login <> ''),
    password_hash TEXT        NOT NULL CHECK (password_hash <> ''),
    role          TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_role_check CHECK (role IN ('admin', 'participant'))
);

-- Sessions store the SHA-256 hash of the token, never the raw token itself.
-- History is never removed together with its owner.
CREATE TABLE sessions (
    token_hash TEXT        NOT NULL,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sessions_pkey PRIMARY KEY (token_hash),
    CONSTRAINT sessions_token_hash_not_empty CHECK (token_hash <> '')
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
