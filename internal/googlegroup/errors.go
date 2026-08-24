package googlegroup

import "errors"

var (
	ErrGroupNotFound          = errors.New("mailing list not found")
	ErrInsufficientPermission = errors.New("insufficient permission to read mailing list members")
	ErrQuotaExceeded          = errors.New("google api quota exceeded")
	ErrNotConfigured          = errors.New("google service account is not configured")
	ErrCredentialsRejected    = errors.New("google rejected the service account credentials")
)
