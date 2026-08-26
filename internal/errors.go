package internal

import (
	"errors"
	"net/http"
	"nycu-sdc/club-manager/internal/apperr"

	"github.com/NYCU-SDC/summer/pkg/problem"
)

func NewProblemWriter() *problem.HttpWriter {
	return problem.NewWithMapping(ErrorHandler)
}

// ErrorHandler maps this project's errors onto RFC 9457 problems. Returning the zero
// Problem falls through to summer's built-in mapping.
func ErrorHandler(err error) problem.Problem {
	switch {
	// Google mailing list errors
	case errors.Is(err, apperr.ErrGroupNotFound):
		return problem.NewNotFoundProblem(err.Error())
	case errors.Is(err, apperr.ErrInsufficientPermission):
		return problem.NewForbiddenProblem(err.Error())
	case errors.Is(err, apperr.ErrQuotaExceeded):
		return NewTooManyRequestsProblem(err.Error())
	case errors.Is(err, apperr.ErrNotConfigured):
		return NewServiceUnavailableProblem("Google integration is not configured on this server")
	case errors.Is(err, apperr.ErrMemberNotFound):
		return problem.NewNotFoundProblem(err.Error())
	case errors.Is(err, apperr.ErrMemberAlreadyExists):
		return NewConflictProblem(err.Error())
	case errors.Is(err, apperr.ErrInvalidMemberRequest):
		return problem.NewValidateProblem(err.Error())
	// Auth errors
	case errors.Is(err, apperr.ErrInvalidState):
		return problem.NewValidateProblem("Invalid or expired login state; start the login again")
	case errors.Is(err, apperr.ErrOAuthExchange):
		return problem.NewValidateProblem("Failed to complete Google sign-in")
	case errors.Is(err, apperr.ErrEmailNotVerified):
		return problem.NewForbiddenProblem("Your Google account email is not verified")
	case errors.Is(err, apperr.ErrNotAMember):
		return problem.NewForbiddenProblem("You are not a member of the group required to sign in")
	case errors.Is(err, apperr.ErrLoginNotConfigured):
		return NewServiceUnavailableProblem("Login is not configured on this server")
	case errors.Is(err, apperr.ErrInvalidRefreshToken):
		return problem.NewUnauthorizedProblem("Refresh token is invalid, expired, or revoked")
	// Google mailing list errors
	case errors.Is(err, apperr.ErrCredentialsRejected):
		return NewServiceUnavailableProblem("Google rejected this server's credentials; check the service account key and domain-wide delegation")
	default:
		return problem.Problem{}
	}
}

func NewTooManyRequestsProblem(detail string) problem.Problem {
	return problem.Problem{
		Title:  "Too Many Requests",
		Status: http.StatusTooManyRequests,
		Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429",
		Detail: detail,
	}
}

func NewConflictProblem(detail string) problem.Problem {
	return problem.Problem{
		Title:  "Conflict",
		Status: http.StatusConflict,
		Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
		Detail: detail,
	}
}

func NewServiceUnavailableProblem(detail string) problem.Problem {
	return problem.Problem{
		Title:  "Service Unavailable",
		Status: http.StatusServiceUnavailable,
		Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/503",
		Detail: detail,
	}
}
