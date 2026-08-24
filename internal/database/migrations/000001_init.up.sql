CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE users
(
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email      TEXT        NOT NULL UNIQUE,
    name       TEXT        NOT NULL DEFAULT '',
    role       TEXT        NOT NULL DEFAULT 'member',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE refresh_tokens
(
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    is_active       BOOLEAN     NOT NULL DEFAULT TRUE,
    expiration_date TIMESTAMPTZ NOT NULL
);

CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);
