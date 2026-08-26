package auth

import (
	"context"
	"errors"
	"testing"

	"nycu-sdc/club-manager/internal/googlegroup"
	"nycu-sdc/club-manager/internal/user"

	"go.uber.org/zap/zaptest"
)

type fakeMembers struct {
	members []googlegroup.Member
	err     error
}

func (f fakeMembers) ListMembers(_ context.Context, _ string) ([]googlegroup.Member, error) {
	return f.members, f.err
}

func newResolver(t *testing.T, members fakeMembers) *RoleResolver {
	t.Helper()
	return NewRoleResolver(zaptest.NewLogger(t), members, "members@example.com")
}

func testRoster() []googlegroup.Member {
	return []googlegroup.Member{
		{Email: "active@example.com", Status: "ACTIVE", Role: "MEMBER"},
		{Email: "Mixed.Case@Example.com", Status: "ACTIVE", Role: "MEMBER"},
		{Email: "suspended@example.com", Status: "SUSPENDED", Role: "OWNER"},
		// External members carry no status at all; see usableMemberStatus.
		{Email: "outsider@nycu.edu.tw", Status: "", Role: "MANAGER"},
		{Email: "owner@example.com", Status: "ACTIVE", Role: "OWNER"},
	}
}

func TestRoleFor(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		wantFound bool
		wantRole  string
	}{
		{"active member", "active@example.com", true, user.RoleMember},
		{"case-insensitive match", "mixed.case@example.com", true, user.RoleMember},
		{"external member with no status", "outsider@nycu.edu.tw", true, user.RoleAdmin},
		{"owner", "owner@example.com", true, user.RoleAdmin},
		// A suspended owner must fail the gate outright rather than fall through to a
		// role check that would hand them admin.
		{"suspended member is refused", "suspended@example.com", false, ""},
		{"unknown address", "stranger@example.com", false, ""},
		{"empty address", "", false, ""},
	}

	r := newResolver(t, fakeMembers{members: testRoster()})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, found, err := r.RoleFor(t.Context(), tt.email)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tt.wantFound {
				t.Fatalf("RoleFor(%q) found = %v, want %v", tt.email, found, tt.wantFound)
			}
			if role != tt.wantRole {
				t.Fatalf("RoleFor(%q) role = %q, want %q", tt.email, role, tt.wantRole)
			}
		})
	}
}

// LocalRoles is what the read paths use, so it must agree with RoleFor on every address
// and key the result in a form email comparisons can rely on.
func TestLocalRoles(t *testing.T) {
	r := newResolver(t, fakeMembers{members: testRoster()})

	roles, err := r.LocalRoles(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"active@example.com":     user.RoleMember,
		"mixed.case@example.com": user.RoleMember,
		"outsider@nycu.edu.tw":   user.RoleAdmin,
		"owner@example.com":      user.RoleAdmin,
	}

	if len(roles) != len(want) {
		t.Fatalf("got %d roles %v, want %d", len(roles), roles, len(want))
	}
	for email, wantRole := range want {
		if roles[email] != wantRole {
			t.Errorf("roles[%q] = %q, want %q", email, roles[email], wantRole)
		}
	}

	// A suspended member is absent rather than present-as-member: callers treat absence
	// as "no privileges", and listing them would imply they are still on the roster.
	if _, ok := roles["suspended@example.com"]; ok {
		t.Error("suspended member must not appear in the role map")
	}
}

// Authority comes from the mailing list and nowhere else, so this mapping is the whole
// definition of who may administer the club through this API.
func TestLocalRoleFor(t *testing.T) {
	tests := []struct {
		groupRole string
		want      string
	}{
		{"OWNER", user.RoleAdmin},
		{"MANAGER", user.RoleAdmin},
		{"owner", user.RoleAdmin},
		{"manager", user.RoleAdmin},
		{"MEMBER", user.RoleMember},
		{"member", user.RoleMember},
		{"", user.RoleMember},
		{"SOMETHING_NEW", user.RoleMember},
	}

	for _, tt := range tests {
		t.Run(tt.groupRole, func(t *testing.T) {
			r := newResolver(t, fakeMembers{})
			if got := r.LocalRoleFor(tt.groupRole); got != tt.want {
				t.Fatalf("LocalRoleFor(%q) = %q, want %q", tt.groupRole, got, tt.want)
			}
		})
	}
}

// A lookup failure must not be mistaken for "not a member" -- but it must also never
// be mistaken for success, so the error has to propagate.
func TestRoleForPropagatesLookupFailure(t *testing.T) {
	sentinel := errors.New("google unavailable")
	r := newResolver(t, fakeMembers{err: sentinel})

	_, got, err := r.RoleFor(t.Context(), "active@example.com")
	if !errors.Is(err, sentinel) {
		t.Fatalf("got error %v, want it to wrap the lookup failure", err)
	}
	if got {
		t.Fatal("a failed lookup must never report membership")
	}
}

func TestLocalRolesPropagatesLookupFailure(t *testing.T) {
	sentinel := errors.New("google unavailable")
	r := newResolver(t, fakeMembers{err: sentinel})

	if _, err := r.LocalRoles(t.Context()); !errors.Is(err, sentinel) {
		t.Fatalf("got error %v, want it to wrap the lookup failure", err)
	}
}
