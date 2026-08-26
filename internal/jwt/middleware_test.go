// External test package: the real problem writer lives in internal, which imports
// nothing from here but is imported by main -- keeping the test outside the package
// keeps it honest about only using the exported surface.
package jwt_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"nycu-sdc/nexmin/internal"
	"nycu-sdc/nexmin/internal/jwt"
	"nycu-sdc/nexmin/internal/user"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"
)

func newRoleMiddleware(t *testing.T) jwt.Middleware {
	t.Helper()
	return jwt.NewMiddleware(nil, zaptest.NewLogger(t), internal.NewProblemWriter())
}

// serveWithRole runs a request through RequireRole with the user already in the context,
// which is what the JWT middleware would have done ahead of it.
func serveWithRole(t *testing.T, ctx context.Context, required ...string) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	reached := false
	handler := newRoleMiddleware(t).RequireRole(required...)(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/groups/team@example.com/members", nil).WithContext(ctx))

	return rec, reached
}

func contextWithRole(role string) context.Context {
	return jwt.SetUserToContext(context.Background(), jwt.User{ID: uuid.New(), Email: "someone@example.com", Role: role})
}

func TestRequireRoleAllowsMatchingRole(t *testing.T) {
	rec, reached := serveWithRole(t, contextWithRole(user.RoleAdmin), user.RoleAdmin)

	if !reached {
		t.Fatal("expected the handler to run")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
}

func TestRequireRoleRejectsOtherRoles(t *testing.T) {
	rec, reached := serveWithRole(t, contextWithRole(user.RoleMember), user.RoleAdmin)

	if reached {
		t.Fatal("the handler must not run for an insufficient role")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("got content-type %q, want application/problem+json", got)
	}
}

// Chained wrongly -- without the JWT middleware in front -- it must fail closed rather
// than treat "no user" as "no restriction".
func TestRequireRoleFailsClosedWithoutUser(t *testing.T) {
	rec, reached := serveWithRole(t, context.Background(), user.RoleAdmin)

	if reached {
		t.Fatal("the handler must not run without a user in the context")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want 403", rec.Code)
	}
}

func TestRequireRoleAcceptsAnyListedRole(t *testing.T) {
	if _, reached := serveWithRole(t, contextWithRole(user.RoleMember), user.RoleAdmin, user.RoleMember); !reached {
		t.Fatal("expected the second listed role to be accepted")
	}
}
