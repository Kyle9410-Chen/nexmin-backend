-- NOTE: user_id deliberately omits the REFERENCES users(id) constraint that the real
-- migration declares. The schema-merge script concatenates schema.sql files in
-- filesystem order, and internal/jwt sorts before internal/user, so a foreign key here
-- would reference a table sqlc has not seen yet. sqlc only needs column types; the
-- actual constraint lives in internal/database/migrations/000001_init.up.sql, where
-- version ordering makes it unambiguous.
CREATE TABLE refresh_tokens
(
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID        NOT NULL,
    is_active       BOOLEAN     NOT NULL DEFAULT TRUE,
    expiration_date TIMESTAMPTZ NOT NULL
);
