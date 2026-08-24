-- name: GetByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1);

-- name: Create :one
INSERT INTO users (email, name) VALUES ($1, $2) RETURNING *;

-- name: UpdateName :one
UPDATE users SET name = $2, updated_at = now() WHERE id = $1 RETURNING *;
