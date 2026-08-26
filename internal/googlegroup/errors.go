package googlegroup

import "nycu-sdc/nexmin/internal/apperr"

// The sentinels themselves live in internal/apperr, a leaf package, so that
// internal/errors.go can map them to RFC 9457 problems without importing this package.
// These aliases keep them addressable as googlegroup.ErrX at the call sites that care
// about mailing lists; errors.Is matches either way, they are the same values.
var (
	ErrGroupNotFound          = apperr.ErrGroupNotFound
	ErrInsufficientPermission = apperr.ErrInsufficientPermission
	ErrQuotaExceeded          = apperr.ErrQuotaExceeded
	ErrNotConfigured          = apperr.ErrNotConfigured
	ErrCredentialsRejected    = apperr.ErrCredentialsRejected

	// Write-only outcomes. Reads can never produce these.
	ErrMemberAlreadyExists  = apperr.ErrMemberAlreadyExists
	ErrMemberNotFound       = apperr.ErrMemberNotFound
	ErrInvalidMemberRequest = apperr.ErrInvalidMemberRequest
)
