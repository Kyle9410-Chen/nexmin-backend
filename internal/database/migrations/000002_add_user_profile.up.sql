-- Profile fields the user maintains themselves. name already existed as the display
-- name seeded from Google at first sign-in; from here on all three belong to the user
-- and are never overwritten by a later OAuth login.
ALTER TABLE users ADD COLUMN nickname   TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN department TEXT NOT NULL DEFAULT '';
