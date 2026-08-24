package auth

import (
	"context"
	"errors"
	"testing"

	"nycu-sdc/club-manager/internal/auth/oauthprovider"
	"nycu-sdc/club-manager/internal/googlegroup"

	"go.uber.org/zap/zaptest"
)

type fakeMembers struct {
	members []googlegroup.Member
	err     error
}

func (f fakeMembers) ListMembers(_ context.Context, _ string) ([]googlegroup.Member, error) {
	return f.members, f.err
}

func newGateHandler(t *testing.T, members fakeMembers) *Handler {
	t.Helper()
	return NewHandler(zaptest.NewLogger(t), nil, members, nil, nil, "members@example.com", "http://frontend", 900)
}

func TestIsMember(t *testing.T) {
	roster := []googlegroup.Member{
		{Email: "active@example.com", Status: "ACTIVE"},
		{Email: "Mixed.Case@Example.com", Status: "ACTIVE"},
		{Email: "suspended@example.com", Status: "SUSPENDED"},
	}

	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{"active member", "active@example.com", true},
		{"case-insensitive match", "mixed.case@example.com", true},
		{"suspended member is refused", "suspended@example.com", false},
		{"unknown address", "stranger@example.com", false},
		{"empty address", "", false},
	}

	h := newGateHandler(t, fakeMembers{members: roster})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := h.isMember(t.Context(), tt.email)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("isMember(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}

// A lookup failure must not be mistaken for "not a member" -- but it must also never
// be mistaken for success, so the error has to propagate.
func TestIsMemberPropagatesLookupFailure(t *testing.T) {
	sentinel := errors.New("google unavailable")
	h := newGateHandler(t, fakeMembers{err: sentinel})

	got, err := h.isMember(t.Context(), "active@example.com")
	if !errors.Is(err, sentinel) {
		t.Fatalf("got error %v, want it to wrap the lookup failure", err)
	}
	if got {
		t.Fatal("a failed lookup must never report membership")
	}
}

func TestConfiguredRequiresEveryPiece(t *testing.T) {
	tests := []struct {
		name        string
		loginGroup  string
		frontendURL string
		want        bool
	}{
		{"missing login group", "", "http://frontend", false},
		{"missing frontend url", "members@example.com", "", false},
		{"all present", "members@example.com", "http://frontend", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fully configured provider, so only the field under test is missing.
			provider := oauthprovider.NewGoogleConfig("client-id", "client-secret", "http://localhost/cb")
			h := NewHandler(zaptest.NewLogger(t), provider, fakeMembers{}, nil, nil, tt.loginGroup, tt.frontendURL, 900)
			if got := h.configured(); got != tt.want {
				t.Fatalf("configured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfiguredRequiresOAuthCredentials(t *testing.T) {
	provider := oauthprovider.NewGoogleConfig("", "", "http://localhost/cb")
	h := NewHandler(zaptest.NewLogger(t), provider, fakeMembers{}, nil, nil, "members@example.com", "http://frontend", 900)

	if h.configured() {
		t.Fatal("expected configured() to be false without OAuth credentials")
	}
}
