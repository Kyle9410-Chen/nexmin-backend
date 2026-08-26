package user

// The roles this service recognizes, stored in users.role and carried in the access
// token's `role` claim.
//
// A user's role is not configured locally: it is derived at sign-in from their role in
// the login mailing list, so the mailing list stays the single place membership and
// authority are administered. See internal/auth.
const (
	// RoleMember is the schema default in schema.sql and 000001_init.up.sql.
	RoleMember = "member"
	RoleAdmin  = "admin"
)
