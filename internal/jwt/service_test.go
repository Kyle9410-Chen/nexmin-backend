package jwt

import (
	"errors"

	"nycu-sdc/nexmin/internal/apperr"

	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"
)

const testSecret = "test-secret"

func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(zaptest.NewLogger(t), testSecret, 15*time.Minute, 24*time.Hour, nil)
}

func signed(t *testing.T, c claims, secret string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to sign test token: %v", err)
	}
	return token
}

func validClaims(sub string) claims {
	return claims{
		Email: "user@example.com",
		Role:  "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{audienceAccess},
			Subject:   sub,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
}

func TestParseValidToken(t *testing.T) {
	s := newTestService(t)
	id := uuid.New()

	user, err := s.Parse(t.Context(), signed(t, validClaims(id.String()), testSecret))
	if err != nil {
		t.Fatalf("expected token to parse, got %v", err)
	}
	if user.ID != id {
		t.Errorf("got id %v, want %v", user.ID, id)
	}
	if user.Email != "user@example.com" {
		t.Errorf("got email %q", user.Email)
	}
	if user.Role != "admin" {
		t.Errorf("got role %q", user.Role)
	}
}

func TestParseAcceptsBearerPrefix(t *testing.T) {
	s := newTestService(t)
	token := signed(t, validClaims(uuid.New().String()), testSecret)

	if _, err := s.Parse(t.Context(), "Bearer "+token); err != nil {
		t.Fatalf("expected Bearer-prefixed token to parse, got %v", err)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	s := newTestService(t)
	token := signed(t, validClaims(uuid.New().String()), "a-different-secret")

	if _, err := s.Parse(t.Context(), token); err == nil {
		t.Fatal("expected a token signed with another secret to be rejected")
	}
}

func TestParseRejectsExpiredToken(t *testing.T) {
	s := newTestService(t)
	c := validClaims(uuid.New().String())
	c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))

	_, err := s.Parse(t.Context(), signed(t, c, testSecret))
	if err == nil {
		t.Fatal("expected an expired token to be rejected")
	}
}

func TestParseRejectsMalformedToken(t *testing.T) {
	s := newTestService(t)

	if _, err := s.Parse(t.Context(), "this-is-not-a-jwt"); err == nil {
		t.Fatal("expected a malformed token to be rejected")
	}
}

func TestParseRejectsNoneAlgorithm(t *testing.T) {
	s := newTestService(t)

	// Algorithm confusion: a token asking for "none" must not be accepted just because
	// it carries well-formed claims.
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims(uuid.New().String())).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to build none-algorithm token: %v", err)
	}

	if _, err := s.Parse(t.Context(), token); err == nil {
		t.Fatal("expected a none-algorithm token to be rejected")
	}
}

func TestParseRejectsNonUUIDSubject(t *testing.T) {
	s := newTestService(t)
	c := validClaims("not-a-uuid")

	if _, err := s.Parse(t.Context(), signed(t, c, testSecret)); err == nil {
		t.Fatal("expected a non-UUID subject to be rejected")
	}
}

func TestNewAndParseRoundTrip(t *testing.T) {
	s := newTestService(t)
	id := uuid.New()

	token, err := s.New(t.Context(), User{ID: id, Email: "a@example.com", Role: "member"})
	if err != nil {
		t.Fatalf("failed to mint token: %v", err)
	}

	got, err := s.Parse(t.Context(), token)
	if err != nil {
		t.Fatalf("failed to parse minted token: %v", err)
	}
	if got.ID != id || got.Email != "a@example.com" || got.Role != "member" {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	s := newTestService(t)

	state, err := s.NewState(t.Context(), "/dashboard")
	if err != nil {
		t.Fatalf("failed to mint state: %v", err)
	}

	redirect, err := s.ParseState(t.Context(), state)
	if err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}
	if redirect != "/dashboard" {
		t.Fatalf("got redirect %q, want /dashboard", redirect)
	}
}

func TestParseStateRejectsForeignSecret(t *testing.T) {
	s := newTestService(t)
	other := NewService(zaptest.NewLogger(t), "another-secret", 15*time.Minute, 24*time.Hour, nil)

	state, err := other.NewState(t.Context(), "/dashboard")
	if err != nil {
		t.Fatalf("failed to mint state: %v", err)
	}

	if _, err := s.ParseState(t.Context(), state); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("got %v, want ErrInvalidState", err)
	}
}

// An access token must not be usable as a state token or vice versa; both are HS256
// with the same secret, so only the claim shape separates them.
func TestParseRejectsStateToken(t *testing.T) {
	s := newTestService(t)

	state, err := s.NewState(t.Context(), "/dashboard")
	if err != nil {
		t.Fatalf("failed to mint state: %v", err)
	}

	user, err := s.Parse(t.Context(), state)
	if err == nil && user.ID != uuid.Nil {
		t.Fatal("a state token was accepted as an access token")
	}
}
