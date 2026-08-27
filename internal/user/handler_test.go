// The handler tests live in the external test package so they exercise the handler
// through its exported surface, the same way main.go and the frontend do.
package user_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nycu-sdc/nexmin/internal"
	"nycu-sdc/nexmin/internal/jwt"
	. "nycu-sdc/nexmin/internal/user"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"
)

// fakeStore records what the handler asked for and replays a canned answer.
type fakeStore struct {
	Store

	user  User
	users []User
	err   error

	gotName       *string
	gotNickname   *string
	gotDepartment *string
	gotRole       string
	roleUpdated   bool
}

func (f *fakeStore) GetByID(_ context.Context, _ uuid.UUID) (User, error) {
	return f.user, f.err
}

func (f *fakeStore) ListByEmails(_ context.Context, _ []string) ([]User, error) {
	return f.users, f.err
}

func (f *fakeStore) UpdateProfile(_ context.Context, _ uuid.UUID, name, nickname, department *string) (User, error) {
	f.gotName, f.gotNickname, f.gotDepartment = name, nickname, department
	if f.err != nil {
		return User{}, f.err
	}

	// Mirror the COALESCE in the real query so the test can tell a partial update from
	// one that blanked the untouched fields.
	updated := f.user
	if name != nil {
		updated.Name = *name
	}
	if nickname != nil {
		updated.Nickname = *nickname
	}
	if department != nil {
		updated.Department = *department
	}

	return updated, nil
}

func (f *fakeStore) UpdateRole(_ context.Context, _ uuid.UUID, role string) (User, error) {
	f.gotRole, f.roleUpdated = role, true
	updated := f.user
	updated.Role = role
	return updated, nil
}

// fakeGroups stands in for membership.Service.
type fakeGroups struct {
	keys   []string
	roster map[string][]string
	err    error

	rosterErr error
}

func (f fakeGroups) GroupKeysForEmail(_ context.Context, _ string) ([]string, error) {
	return f.keys, f.err
}

func (f fakeGroups) RosterGroupKeys(_ context.Context) (map[string][]string, error) {
	return f.roster, f.rosterErr
}

// fakeRoles stands in for auth.RoleResolver.
type fakeRoles struct {
	roles map[string]string
	err   error
}

func (f fakeRoles) LocalRoles(_ context.Context) (map[string]string, error) {
	return f.roles, f.err
}

// Mirrors auth.RoleResolver.LocalRoleFor: OWNER and MANAGER of the login group are this
// service's admins.
func (f fakeRoles) LocalRoleFor(groupRole string) string {
	switch strings.ToUpper(groupRole) {
	case "OWNER", "MANAGER":
		return RoleAdmin
	default:
		return RoleMember
	}
}

// fakeRoster stands in for membership.Service's write side.
type fakeRoster struct {
	groups []string
	err    error

	// loginRole is what the service reports having applied on the login group. Empty
	// stands for "the membership already existed", which is what makes the handler look
	// the current role up instead.
	loginRole string
	unwritten bool

	addedEmail  string
	addedGroups []GroupRole
	removed     string
}

func (f *fakeRoster) AddToRoster(_ context.Context, email string, groups []GroupRole) ([]string, string, error) {
	f.addedEmail, f.addedGroups = email, groups
	if f.err != nil {
		return nil, "", f.err
	}

	if f.unwritten {
		return f.groups, "", nil
	}

	role := f.loginRole
	if role == "" {
		role = "MEMBER"
	}

	return f.groups, role, nil
}

func (f *fakeRoster) RemoveFromRoster(_ context.Context, email string) error {
	f.removed = email
	return f.err
}

const callerID = "6f1d0f6c-3b1f-4a5a-9f1a-2b0c9d8e7f60"

func serve(t *testing.T, store Store, roles RoleResolver, method, target, body string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	return serveWithGroups(t, store, roles, fakeGroups{}, method, target, body, authenticated)
}

func serveWithGroups(t *testing.T, store Store, roles RoleResolver, groups MembershipLister, method, target, body string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	return serveWithRoster(t, store, roles, groups, &fakeRoster{}, method, target, body, authenticated)
}

func serveWithRoster(t *testing.T, store Store, roles RoleResolver, groups MembershipLister, roster RosterWriter, method, target, body string, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()

	h := NewHandler(zaptest.NewLogger(t), validator.New(), internal.NewProblemWriter(), store, roles, groups, roster)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users/me", h.MeHandler)
	mux.HandleFunc("PATCH /api/users/me", h.UpdateMeHandler)
	mux.HandleFunc("GET /api/users", h.ListHandler)
	mux.HandleFunc("POST /api/users", h.AddHandler)
	mux.HandleFunc("DELETE /api/users/{email}", h.RemoveHandler)
	mux.HandleFunc("GET /api/users/{user_id}", h.GetHandler)

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		// The JWT middleware is what normally puts this here.
		req = req.WithContext(jwt.SetUserToContext(req.Context(), jwt.User{ID: uuid.MustParse(callerID)}))
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) Response {
	t.Helper()

	var got Response
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}

	return got
}

func storedUser() User {
	return User{
		ID:         uuid.MustParse(callerID),
		Email:      "kai@example.com",
		Name:       "Kai Chen",
		Nickname:   "Kai",
		Department: "CSIE",
		Role:       RoleMember,
	}
}

func memberRoles() fakeRoles {
	return fakeRoles{roles: map[string]string{"kai@example.com": RoleMember}}
}

// A PATCH naming one field must leave the others alone -- the pointer fields are the
// whole reason the request DTO is shaped the way it is.
func TestUpdateMeHandlerAppliesPartialUpdate(t *testing.T) {
	store := &fakeStore{user: storedUser()}
	rec := serve(t, store, memberRoles(), http.MethodPatch, "/api/users/me", `{"nickname":"KK"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotName != nil || store.gotDepartment != nil {
		t.Fatalf("omitted fields must stay nil, got name=%v department=%v", store.gotName, store.gotDepartment)
	}
	if store.gotNickname == nil || *store.gotNickname != "KK" {
		t.Fatalf("got nickname %v, want KK", store.gotNickname)
	}

	got := decode(t, rec)
	if got.Nickname != "KK" || got.Name != "Kai Chen" || got.Department != "CSIE" {
		t.Fatalf("unexpected body: %+v", got)
	}
}

// Nickname and department may be cleared; sending "" is a real request, not an omission.
func TestUpdateMeHandlerClearsFieldOnEmptyString(t *testing.T) {
	store := &fakeStore{user: storedUser()}
	rec := serve(t, store, memberRoles(), http.MethodPatch, "/api/users/me", `{"nickname":""}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotNickname == nil || *store.gotNickname != "" {
		t.Fatalf("got nickname %v, want an explicit empty string", store.gotNickname)
	}
}

func TestUpdateMeHandlerTrimsWhitespace(t *testing.T) {
	store := &fakeStore{user: storedUser()}
	rec := serve(t, store, memberRoles(), http.MethodPatch, "/api/users/me", `{"department":"  CSIE  "}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotDepartment == nil || *store.gotDepartment != "CSIE" {
		t.Fatalf("got department %v, want it trimmed to CSIE", store.gotDepartment)
	}
}

func TestUpdateMeHandlerRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		// Name identifies the member in every roster that displays it, so clearing it
		// is refused rather than silently accepted.
		{"blank name", `{"name":"   "}`},
		{"name too long", `{"name":"` + strings.Repeat("a", MaxNameLength+1) + `"}`},
		{"nickname too long", `{"nickname":"` + strings.Repeat("a", MaxNicknameLength+1) + `"}`},
		{"department too long", `{"department":"` + strings.Repeat("a", MaxDepartmentLength+1) + `"}`},
		{"malformed body", `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &fakeStore{user: storedUser()}, memberRoles(), http.MethodPatch, "/api/users/me", tt.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("got content-type %q, want application/problem+json", got)
			}
		})
	}
}

// Length is counted in runes: a three-character Chinese name is three, not nine.
func TestNormalizeProfileFieldCountsRunes(t *testing.T) {
	value := strings.Repeat("陳", MaxNicknameLength)
	if _, err := NormalizeProfileField("nickname", value, MaxNicknameLength); err != nil {
		t.Fatalf("unexpected error for a %d-rune value: %v", MaxNicknameLength, err)
	}
}

func TestMeHandlerRequiresAuthenticatedUser(t *testing.T) {
	rec := serve(t, &fakeStore{user: storedUser()}, memberRoles(), http.MethodGet, "/api/users/me", "", false)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}

func TestGetHandlerRejectsMalformedID(t *testing.T) {
	rec := serve(t, &fakeStore{user: storedUser()}, memberRoles(), http.MethodGet, "/api/users/not-a-uuid", "", true)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400; body %s", rec.Code, rec.Body.String())
	}
}

// The mailing list is the authority: a promotion there shows up immediately, without
// waiting for the member to sign in again, and is written back to the cached column.
func TestResponseReportsPromotionFromLoginGroup(t *testing.T) {
	store := &fakeStore{user: storedUser()}
	roles := fakeRoles{roles: map[string]string{"kai@example.com": RoleAdmin}}

	rec := serve(t, store, roles, http.MethodGet, "/api/users/me", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	if got := decode(t, rec); got.Role != RoleAdmin {
		t.Fatalf("got role %q, want %q", got.Role, RoleAdmin)
	}
	if !store.roleUpdated || store.gotRole != RoleAdmin {
		t.Fatalf("expected the resolved role to be written back, got updated=%v role=%q", store.roleUpdated, store.gotRole)
	}
}

// Someone removed from the login group keeps their local row but loses every privilege,
// so absence from the roster demotes rather than preserving the stored role.
func TestResponseDemotesAddressMissingFromLoginGroup(t *testing.T) {
	stored := storedUser()
	stored.Role = RoleAdmin
	store := &fakeStore{user: stored}

	rec := serve(t, store, fakeRoles{roles: map[string]string{}}, http.MethodGet, "/api/users/me", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	if got := decode(t, rec); got.Role != RoleMember {
		t.Fatalf("got role %q, want %q", got.Role, RoleMember)
	}
	if !store.roleUpdated || store.gotRole != RoleMember {
		t.Fatalf("expected the demotion to be written back, got updated=%v role=%q", store.roleUpdated, store.gotRole)
	}
}

func TestResponseSkipsWriteBackWhenRoleAgrees(t *testing.T) {
	store := &fakeStore{user: storedUser()}

	rec := serve(t, store, memberRoles(), http.MethodGet, "/api/users/me", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.roleUpdated {
		t.Fatal("an unchanged role must not cause a write")
	}
}

// Google credentials are optional by design, so an unreachable or unconfigured
// Directory API must degrade to the stored role instead of taking the endpoint down.
func TestResponseFallsBackToStoredRoleWhenResolverFails(t *testing.T) {
	stored := storedUser()
	stored.Role = RoleAdmin
	store := &fakeStore{user: stored}

	rec := serve(t, store, fakeRoles{err: errors.New("google unavailable")}, http.MethodGet, "/api/users/me", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	if got := decode(t, rec); got.Role != RoleAdmin {
		t.Fatalf("got role %q, want the stored %q", got.Role, RoleAdmin)
	}
	if store.roleUpdated {
		t.Fatal("a failed lookup must not rewrite the stored role")
	}
}

// The compact list rides along on the caller's own profile so the page can render chips
// without a second round trip.
func TestMeHandlerCarriesGroupKeys(t *testing.T) {
	groups := fakeGroups{keys: []string{"engineering", "general"}}
	rec := serveWithGroups(t, &fakeStore{user: storedUser()}, memberRoles(), groups,
		http.MethodGet, "/api/users/me", "", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	got := decode(t, rec)
	if len(got.Groups) != 2 || got.Groups[0] != "engineering" || got.Groups[1] != "general" {
		t.Fatalf("got groups %v, want [engineering general]", got.Groups)
	}
}

// Google credentials are optional, so the profile must still load without them -- with
// groups null rather than an empty list, so the frontend can tell "unknown" from "none".
func TestMeHandlerOmitsGroupsWhenLookupFails(t *testing.T) {
	groups := fakeGroups{err: errors.New("google unavailable")}
	rec := serveWithGroups(t, &fakeStore{user: storedUser()}, memberRoles(), groups,
		http.MethodGet, "/api/users/me", "", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec); got.Groups != nil {
		t.Fatalf("got groups %v, want null", got.Groups)
	}
}

func rosterStore() *fakeStore {
	signedIn := storedUser()
	return &fakeStore{users: []User{signedIn}}
}

func rosterGroups() fakeGroups {
	return fakeGroups{roster: map[string][]string{
		"kai@example.com":      {"general", "engineering"},
		"newcomer@example.com": {"general"},
	}}
}

// The roster comes from the mailing list; the local row only fills in the profile.
func TestListHandlerReturnsTheRoster(t *testing.T) {
	roles := fakeRoles{roles: map[string]string{
		"kai@example.com":      RoleAdmin,
		"newcomer@example.com": RoleMember,
	}}

	rec := serveWithGroups(t, rosterStore(), roles, rosterGroups(), http.MethodGet, "/api/users", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	var got RosterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}
	if got.TotalItems != 2 {
		t.Fatalf("got %d entries, want everyone on the login group", got.TotalItems)
	}

	byEmail := map[string]RosterEntry{}
	for _, e := range got.Items {
		byEmail[strings.ToLower(e.Email)] = e
	}

	signedIn := byEmail["kai@example.com"]
	if signedIn.Profile == nil {
		t.Fatal("someone who has signed in must carry their profile")
	}
	if signedIn.Profile.Nickname != "Kai" || signedIn.Profile.Department != "CSIE" {
		t.Fatalf("unexpected profile: %+v", *signedIn.Profile)
	}
	if signedIn.Role != RoleAdmin {
		t.Fatalf("got role %q, want it from the login group", signedIn.Role)
	}
	if len(signedIn.Groups) != 2 {
		t.Fatalf("got groups %v, want the mailing lists they reach", signedIn.Groups)
	}

	// On the mailing list, never signed in: present, but with no profile.
	newcomer := byEmail["newcomer@example.com"]
	if newcomer.Profile != nil {
		t.Fatalf("expected a null profile, got %+v", *newcomer.Profile)
	}
	if newcomer.Role != RoleMember {
		t.Fatalf("got role %q, want member", newcomer.Role)
	}
}

// A roster is read by people, so it sorts by the name they go by and falls back to the
// address for anyone who has not filled one in.
func TestListHandlerSortsByDisplayedName(t *testing.T) {
	groups := fakeGroups{roster: map[string][]string{
		"kai@example.com":   {"general"},
		"aaron@example.com": {"general"},
	}}
	roles := fakeRoles{roles: map[string]string{"kai@example.com": RoleMember, "aaron@example.com": RoleMember}}

	rec := serveWithGroups(t, rosterStore(), roles, groups, http.MethodGet, "/api/users", "", true)

	var got RosterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}
	// "aaron@example.com" (no profile, sorts on its address) before "Kai Chen".
	if got.Items[0].Email != "aaron@example.com" {
		t.Fatalf("got %q first, want aaron@example.com", got.Items[0].Email)
	}
}

// Everything on this endpoint comes from Google, so unlike /users/me there is nothing
// to fall back to.
func TestListHandlerFailsWhenTheRosterIsUnavailable(t *testing.T) {
	groups := fakeGroups{rosterErr: errors.New("google unavailable")}

	rec := serveWithGroups(t, rosterStore(), memberRoles(), groups, http.MethodGet, "/api/users", "", true)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected the request to fail, got 200: %s", rec.Body.String())
	}
}

func decodeEntry(t *testing.T, rec *httptest.ResponseRecorder) RosterEntry {
	t.Helper()

	var got RosterEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}

	return got
}

// Adding someone to the club creates no local row -- one appears when they first sign
// in -- so the entry comes back with a null profile.
func TestAddHandlerReturnsTheNewEntry(t *testing.T) {
	roster := &fakeRoster{groups: []string{"general"}}

	rec := serveWithRoster(t, &fakeStore{}, memberRoles(), fakeGroups{}, roster,
		http.MethodPost, "/api/users", `{"email":"newcomer@example.com"}`, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if roster.addedEmail != "newcomer@example.com" {
		t.Fatalf("store saw email %q", roster.addedEmail)
	}

	got := decodeEntry(t, rec)
	if got.Profile != nil {
		t.Fatalf("expected a null profile, got %+v", *got.Profile)
	}
	if got.Role != RoleMember {
		t.Fatalf("got role %q, want member", got.Role)
	}
	if strings.Join(got.Groups, ",") != "general" {
		t.Fatalf("got groups %v, want the login group", got.Groups)
	}
}

// Naming the login group with a role of MANAGER is this service's admin. The endpoint is
// called "add a user", so this consequence needs pinning down.
func TestAddHandlerWithManagerRoleGrantsAdmin(t *testing.T) {
	roster := &fakeRoster{groups: []string{"general"}, loginRole: "MANAGER"}

	rec := serveWithRoster(t, &fakeStore{}, memberRoles(), fakeGroups{}, roster,
		http.MethodPost, "/api/users", `{"email":"officer@example.com","groups":[{"key":"general","role":"MANAGER"}]}`, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeEntry(t, rec); got.Role != RoleAdmin {
		t.Fatalf("got role %q, want admin", got.Role)
	}
}

// The lists reach the roster writer exactly as asked for, in order: the service decides
// what to do with them, the handler only carries them across.
func TestAddHandlerForwardsEveryRequestedList(t *testing.T) {
	roster := &fakeRoster{groups: []string{"general", "engineering"}}

	rec := serveWithRoster(t, &fakeStore{}, memberRoles(), fakeGroups{}, roster,
		http.MethodPost, "/api/users",
		`{"email":"newcomer@example.com","groups":[{"key":"engineering","role":"MANAGER"},{"key":"design"}]}`, true)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	want := []GroupRole{{Key: "engineering", Role: "MANAGER"}, {Key: "design"}}
	if len(roster.addedGroups) != len(want) {
		t.Fatalf("got %v, want %v", roster.addedGroups, want)
	}
	for i := range want {
		if roster.addedGroups[i] != want[i] {
			t.Fatalf("got %v, want %v", roster.addedGroups, want)
		}
	}
}

// Adding someone who was already on the login group writes nothing there, so the role
// they hold has to be read rather than derived from what the request asked for.
func TestAddHandlerReportsTheLiveRoleWhenNothingWasWritten(t *testing.T) {
	roster := &fakeRoster{groups: []string{"general"}, unwritten: true}
	roles := fakeRoles{roles: map[string]string{"officer@example.com": RoleAdmin}}

	rec := serveWithRoster(t, &fakeStore{}, roles, fakeGroups{}, roster,
		http.MethodPost, "/api/users", `{"email":"officer@example.com","groups":[{"key":"engineering"}]}`, true)

	if got := decodeEntry(t, rec); got.Role != RoleAdmin {
		t.Fatalf("got role %q, want the role they already hold", got.Role)
	}
}

// Someone removed and then restored still has the profile they filled in before.
func TestAddHandlerCarriesAnExistingProfile(t *testing.T) {
	roster := &fakeRoster{groups: []string{"general"}}
	store := &fakeStore{users: []User{storedUser()}}

	rec := serveWithRoster(t, store, memberRoles(), fakeGroups{}, roster,
		http.MethodPost, "/api/users", `{"email":"kai@example.com"}`, true)

	got := decodeEntry(t, rec)
	if got.Profile == nil || got.Profile.Nickname != "Kai" {
		t.Fatalf("expected the existing profile to come back, got %+v", got.Profile)
	}
}

func TestAddHandlerRejectsBadRequests(t *testing.T) {
	tests := []struct{ name, body string }{
		{"missing email", `{}`},
		{"malformed email", `{"email":"not-an-email"}`},
		{"group without a key", `{"email":"x@example.com","groups":[{"role":"MEMBER"}]}`},
		{"malformed body", `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roster := &fakeRoster{}
			rec := serveWithRoster(t, &fakeStore{}, memberRoles(), fakeGroups{}, roster,
				http.MethodPost, "/api/users", tt.body, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if roster.addedEmail != "" {
				t.Fatal("a rejected request must not reach the mailing list")
			}
		})
	}
}

func TestRemoveHandlerReturnsEmpty204(t *testing.T) {
	roster := &fakeRoster{}

	rec := serveWithRoster(t, &fakeStore{}, memberRoles(), fakeGroups{}, roster,
		http.MethodDelete, "/api/users/leaver@example.com", "", true)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body, got %q", rec.Body.String())
	}
	if roster.removed != "leaver@example.com" {
		t.Fatalf("store saw %q", roster.removed)
	}
}
