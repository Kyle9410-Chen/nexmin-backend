-- name: GetByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1);

-- name: Create :one
INSERT INTO users (email, name, role) VALUES ($1, $2, $3) RETURNING *;

-- name: ListByEmails :many
SELECT * FROM users WHERE lower(email) = ANY($1::text[]);

-- name: UpdateRole :one
UPDATE users SET role = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- Partial update: a NULL argument leaves the stored value alone, so PATCH can send
-- only the fields it means to change without a read-modify-write round trip.
-- name: UpdateProfile :one
UPDATE users
SET name       = COALESCE(sqlc.narg('name'), name),
    nickname   = COALESCE(sqlc.narg('nickname'), nickname),
    department = COALESCE(sqlc.narg('department'), department),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- Creates a row for someone who has not signed in yet, so the roster can show a real
-- person instead of a bare address. An existing row is left exactly as it is: once
-- somebody has signed in, their profile is theirs to edit.
--
-- The role column is deliberately absent so its default applies. It caches the mailing
-- list role, recomputed on every sign-in and on the user read paths; seeding a guess
-- here would only be wrong until the first of those runs.
-- name: SeedProfile :execrows
INSERT INTO users (email, name, nickname)
VALUES ($1, $2, $3)
ON CONFLICT (email) DO NOTHING;
