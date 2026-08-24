// Package apperr holds sentinel errors shared across packages.
//
// It exists to break an import cycle: internal/errors.go maps errors to RFC 9457
// problems and would otherwise need to import internal/jwt and internal/auth, but
// those already import internal for the request-context key. Keeping the sentinels in
// a leaf package with no internal imports lets both sides reference them.
package apperr

import "errors"

var (
	// Auth / OAuth
	ErrInvalidState       = errors.New("invalid or expired oauth state")
	ErrOAuthExchange      = errors.New("failed to exchange authorization code")
	ErrEmailNotVerified   = errors.New("google account email is not verified")
	ErrNotAMember         = errors.New("user is not a member of the login group")
	ErrLoginNotConfigured = errors.New("login is not configured on this server")

	// Session
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
)
