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

	// Google mailing list. They live here rather than in internal/googlegroup so that
	// internal/errors.go can map them without importing that package: internal/jwt
	// imports internal for the context key, and internal/googlegroup imports
	// internal/user to attach profiles, so an internal -> googlegroup edge would close
	// a cycle the moment internal/user needs anything from internal/jwt.
	ErrGroupNotFound          = errors.New("mailing list not found")
	ErrInsufficientPermission = errors.New("insufficient permission to access mailing list")
	ErrQuotaExceeded          = errors.New("google api quota exceeded")
	ErrNotConfigured          = errors.New("google service account is not configured")
	ErrCredentialsRejected    = errors.New("google rejected the service account credentials")

	// Write-only outcomes. Reads can never produce these.
	ErrMemberAlreadyExists  = errors.New("member already exists in the mailing list")
	ErrMemberNotFound       = errors.New("mailing list member not found")
	ErrInvalidMemberRequest = errors.New("google rejected the member request")
)
