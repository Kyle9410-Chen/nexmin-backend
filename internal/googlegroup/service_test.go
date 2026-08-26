package googlegroup

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
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

func TestTranslateMemberAPIError(t *testing.T) {
	tests := []struct {
		name string
		code int
		want error
	}{
		// 404 on a member call can mean either the group or the member is missing, so it
		// maps to the member sentinel and the message names both.
		{"not found", http.StatusNotFound, ErrMemberNotFound},
		{"conflict", http.StatusConflict, ErrMemberAlreadyExists},
		{"bad request", http.StatusBadRequest, ErrInvalidMemberRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := translateMemberAPIError(&googleapi.Error{Code: tt.code, Message: "invalid role"}, "group@example.com", "user@example.com")
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

func TestTranslateMemberAPIErrorNamesBothKeysOn404(t *testing.T) {
	err := translateMemberAPIError(&googleapi.Error{Code: http.StatusNotFound}, "group@example.com", "user@example.com")
	for _, want := range []string{"group@example.com", "user@example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q in the error, got %q", want, err.Error())
		}
	}
}

// Everything a read can also produce must keep its existing meaning.
func TestTranslateMemberAPIErrorDelegatesSharedCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"forbidden", &googleapi.Error{Code: http.StatusForbidden}, ErrInsufficientPermission},
		{"rate limited", &googleapi.Error{Code: http.StatusTooManyRequests}, ErrQuotaExceeded},
		{"credentials rejected", &oauth2.RetrieveError{ErrorCode: "unauthorized_client"}, ErrCredentialsRejected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := translateMemberAPIError(tt.err, "group@example.com", "user@example.com")
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

func TestNormalizeRole(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"MEMBER", RoleMember, false},
		{"manager", RoleManager, false},
		{" Owner ", RoleOwner, false},
		// Empty means MEMBER, which is Google's own default for members.insert.
		{"", RoleMember, false},
		{"ADMIN", "", true},
		{"OWNERS", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeRole(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeRole(%q) = %q, want an error", tt.in, got)
				}
				if !errors.Is(err, handlerutil.ErrValidation) {
					t.Fatalf("got error %v, want a validation error so it maps to 400", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeRole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWriteMethodsRequireConfiguration(t *testing.T) {
	s, err := NewService(testLogger(t), Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := s.AddMember(t.Context(), "group@example.com", "user@example.com", RoleMember); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("AddMember: got %v, want ErrNotConfigured", err)
	}
	if _, err := s.UpdateMemberRole(t.Context(), "group@example.com", "user@example.com", RoleManager); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("UpdateMemberRole: got %v, want ErrNotConfigured", err)
	}
	if err := s.RemoveMember(t.Context(), "group@example.com", "user@example.com"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("RemoveMember: got %v, want ErrNotConfigured", err)
	}
}
