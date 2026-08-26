package auth

import (
	"testing"

	"nycu-sdc/club-manager/internal/auth/oauthprovider"

	"go.uber.org/zap/zaptest"
)

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
			roles := NewRoleResolver(zaptest.NewLogger(t), fakeMembers{}, tt.loginGroup)
			h := NewHandler(zaptest.NewLogger(t), provider, roles, nil, nil, tt.frontendURL, 900)
			if got := h.configured(); got != tt.want {
				t.Fatalf("configured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfiguredRequiresOAuthCredentials(t *testing.T) {
	provider := oauthprovider.NewGoogleConfig("", "", "http://localhost/cb")
	roles := NewRoleResolver(zaptest.NewLogger(t), fakeMembers{}, "members@example.com")
	h := NewHandler(zaptest.NewLogger(t), provider, roles, nil, nil, "http://frontend", 900)

	if h.configured() {
		t.Fatal("expected configured() to be false without OAuth credentials")
	}
}
