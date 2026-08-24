package googlegroup

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestTranslateAPIError(t *testing.T) {
	tests := []struct {
		name string
		code int
		want error
	}{
		{"not found", http.StatusNotFound, ErrGroupNotFound},
		{"unauthorized", http.StatusUnauthorized, ErrInsufficientPermission},
		{"forbidden", http.StatusForbidden, ErrInsufficientPermission},
		{"rate limited", http.StatusTooManyRequests, ErrQuotaExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := translateAPIError(&googleapi.Error{Code: tt.code}, "group@example.com")
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want it to wrap %v", err, tt.want)
			}
			// The group key must survive so the problem detail is actionable.
			if got := err.Error(); !strings.Contains(got, "group@example.com") {
				t.Fatalf("expected group key in error, got %q", got)
			}
		})
	}
}

func TestTranslateAPIErrorPassesThroughUnknownCodes(t *testing.T) {
	original := &googleapi.Error{Code: http.StatusInternalServerError}
	if got := translateAPIError(original, "group@example.com"); !errors.Is(got, original) {
		t.Fatalf("expected a 500 to pass through unchanged, got %v", got)
	}
}

func TestTranslateAPIErrorPassesThroughNonAPIErrors(t *testing.T) {
	original := errors.New("dial tcp: connection refused")
	if got := translateAPIError(original, "group@example.com"); !errors.Is(got, original) {
		t.Fatalf("expected a transport error to pass through unchanged, got %v", got)
	}
}

func TestNewServiceWithoutKeyIsUnconfigured(t *testing.T) {
	s, err := NewService(testLogger(t), Config{})
	if err != nil {
		t.Fatalf("expected no error without credentials, got %v", err)
	}
	if s.configured {
		t.Fatal("expected service to report itself unconfigured")
	}
	if _, err := s.ListMembers(t.Context(), "group@example.com"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ListMembers: got %v, want ErrNotConfigured", err)
	}
	if _, err := s.ListGroups(t.Context()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ListGroups: got %v, want ErrNotConfigured", err)
	}
}

func TestNewServiceRejectsBadCacheTTL(t *testing.T) {
	if _, err := NewService(testLogger(t), Config{CacheTTL: "not-a-duration"}); err == nil {
		t.Fatal("expected an error for an unparseable cache TTL")
	}
}

func TestNewServiceRejectsKeyWithoutSubject(t *testing.T) {
	// Valid base64, but no impersonation subject: the Admin SDK would otherwise fail
	// later with an opaque 400.
	_, err := NewService(testLogger(t), Config{ServiceAccountKey: "e30="})
	if err == nil {
		t.Fatal("expected an error when impersonate_subject is missing")
	}
}

func TestNewServiceRejectsNonBase64Key(t *testing.T) {
	_, err := NewService(testLogger(t), Config{ServiceAccountKey: "!!!not base64!!!", ImpersonateSubject: "admin@example.com"})
	if err == nil {
		t.Fatal("expected an error for a non-base64 service account key")
	}
}

func TestTranslateAPIErrorMapsTokenFailures(t *testing.T) {
	// A credentials/DWD failure arrives as an oauth2 retrieve error, not a googleapi
	// error, and must not be reported as a generic 500.
	err := translateAPIError(&oauth2.RetrieveError{ErrorCode: "unauthorized_client"}, "group@example.com")
	if !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("got %v, want it to wrap ErrCredentialsRejected", err)
	}
	if !strings.Contains(err.Error(), "unauthorized_client") {
		t.Fatalf("expected the oauth2 error code in the message, got %q", err.Error())
	}
}
