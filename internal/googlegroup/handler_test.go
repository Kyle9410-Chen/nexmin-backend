// The handler tests live in the external test package so they exercise the handler
// through its exported surface, the same way main.go and the frontend do.
package googlegroup_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nycu-sdc/nexmin/internal"
	. "nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/orgchart"
	"nycu-sdc/nexmin/internal/user"

	"github.com/go-playground/validator/v10"
	"go.uber.org/zap/zaptest"
)

// fakeStore records what the handler asked for and replays a canned answer, so the
// tests cover request parsing, validation and status codes without a Google client.
type fakeStore struct {
	Store

	member  Member
	members []Member
	groups  []Group
	err     error

	gotGroupKey  string
	gotMemberKey string
	gotEmail     string
	gotRole      string
	removeCalled bool
}

func (f *fakeStore) ListGroups(_ context.Context) ([]Group, error) {
	return f.groups, f.err
}

func (f *fakeStore) ListMembers(_ context.Context, groupKey string) ([]Member, error) {
	f.gotGroupKey = groupKey
	return f.members, f.err
}

func (f *fakeStore) AddMember(_ context.Context, groupKey, email, role string) (Member, error) {
	f.gotGroupKey, f.gotEmail, f.gotRole = groupKey, email, role
	return f.member, f.err
}

func (f *fakeStore) UpdateMemberRole(_ context.Context, groupKey, memberKey, role string) (Member, error) {
	f.gotGroupKey, f.gotMemberKey, f.gotRole = groupKey, memberKey, role
	return f.member, f.err
}

func (f *fakeStore) RemoveMember(_ context.Context, groupKey, memberKey string) error {
	f.gotGroupKey, f.gotMemberKey = groupKey, memberKey
	f.removeCalled = true
	return f.err
}

// fakeProfiles stands in for the local user store the handler joins members against.
type fakeProfiles struct {
	users []user.User
	err   error

	gotEmails []string
}

func (f *fakeProfiles) ListByEmails(_ context.Context, emails []string) ([]user.User, error) {
	f.gotEmails = emails
	return f.users, f.err
}

// serve routes through a mux so the {group_key}/{member_key} wildcards are populated the
// same way they are in main.go.
func serve(t *testing.T, store Store, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveWithProfiles(t, store, &fakeProfiles{}, method, target, body)
}

func serveWithProfiles(t *testing.T, store Store, profiles ProfileStore, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	// The real chart, not a fake: this doubles as a check that the committed
	// classification actually renders.
	chart, err := orgchart.Load()
	if err != nil {
		t.Fatalf("chart: %v", err)
	}

	h := NewHandler(zaptest.NewLogger(t), validator.New(), internal.NewProblemWriter(), store, profiles, chart)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/groups", h.ListGroupsHandler)
	mux.HandleFunc("GET /api/groups/{group_key}/members", h.ListMembersHandler)
	mux.HandleFunc("POST /api/groups/{group_key}/members", h.AddMemberHandler)
	mux.HandleFunc("PATCH /api/groups/{group_key}/members/{member_key}", h.UpdateMemberHandler)
	mux.HandleFunc("DELETE /api/groups/{group_key}/members/{member_key}", h.RemoveMemberHandler)

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

func decodeMember(t *testing.T, rec *httptest.ResponseRecorder) MemberResponse {
	t.Helper()

	var got MemberResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}

	return got
}

func TestAddMemberHandler(t *testing.T) {
	store := &fakeStore{member: Member{ID: "1", Email: "new@example.com", Role: RoleMember, Type: "USER"}}
	rec := serve(t, store, http.MethodPost, "/api/groups/team@example.com/members", `{"email":"new@example.com"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotGroupKey != "team@example.com" || store.gotEmail != "new@example.com" {
		t.Fatalf("store saw group %q email %q", store.gotGroupKey, store.gotEmail)
	}
	// An omitted role must reach the API as MEMBER rather than an empty string.
	if store.gotRole != RoleMember {
		t.Fatalf("got role %q, want %q", store.gotRole, RoleMember)
	}
	if got := decodeMember(t, rec); got.Email != "new@example.com" {
		t.Fatalf("unexpected body: %+v", got)
	}
}

// External members come back from Google with no status at all, so an empty status in a
// 201 is a normal result and must not be treated as a failure.
func TestAddMemberHandlerAcceptsEmptyStatus(t *testing.T) {
	store := &fakeStore{member: Member{Email: "outsider@nycu.edu.tw", Role: RoleMember, Status: ""}}
	rec := serve(t, store, http.MethodPost, "/api/groups/team@example.com/members", `{"email":"outsider@nycu.edu.tw"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeMember(t, rec); got.Status != "" {
		t.Fatalf("got status %q, want it preserved as empty", got.Status)
	}
}

func TestAddMemberHandlerNormalizesRole(t *testing.T) {
	store := &fakeStore{member: Member{Email: "new@example.com", Role: RoleManager}}
	rec := serve(t, store, http.MethodPost, "/api/groups/team@example.com/members", `{"email":"new@example.com","role":"manager"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got status %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if store.gotRole != RoleManager {
		t.Fatalf("got role %q, want %q", store.gotRole, RoleManager)
	}
}

func TestUpdateMemberHandler(t *testing.T) {
	store := &fakeStore{member: Member{Email: "someone@example.com", Role: RoleManager}}
	rec := serve(t, store, http.MethodPatch, "/api/groups/team@example.com/members/someone@example.com", `{"role":"MANAGER"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if store.gotMemberKey != "someone@example.com" || store.gotRole != RoleManager {
		t.Fatalf("store saw member %q role %q", store.gotMemberKey, store.gotRole)
	}
}

func TestRemoveMemberHandlerReturnsEmpty204(t *testing.T) {
	store := &fakeStore{}
	rec := serve(t, store, http.MethodDelete, "/api/groups/team@example.com/members/someone@example.com", "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	// WriteJSONResponse would have marshalled a `null` here.
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body, got %q", rec.Body.String())
	}
	if !store.removeCalled {
		t.Fatal("expected the store to be called")
	}
}

func TestWriteHandlerRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"missing email", http.MethodPost, "/api/groups/team@example.com/members", `{}`},
		{"malformed email", http.MethodPost, "/api/groups/team@example.com/members", `{"email":"not-an-email"}`},
		{"unknown role on add", http.MethodPost, "/api/groups/team@example.com/members", `{"email":"a@example.com","role":"ADMIN"}`},
		{"malformed body", http.MethodPost, "/api/groups/team@example.com/members", `{`},
		{"missing role on update", http.MethodPatch, "/api/groups/team@example.com/members/a@example.com", `{}`},
		{"unknown role on update", http.MethodPatch, "/api/groups/team@example.com/members/a@example.com", `{"role":"ADMIN"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &fakeStore{}, tt.method, tt.target, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWriteHandlerMapsStoreErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"already a member", ErrMemberAlreadyExists, http.StatusConflict},
		{"no such member", ErrMemberNotFound, http.StatusNotFound},
		{"no such group", ErrGroupNotFound, http.StatusNotFound},
		{"rejected by google", ErrInvalidMemberRequest, http.StatusBadRequest},
		{"read-only scope", ErrInsufficientPermission, http.StatusForbidden},
		{"unconfigured", ErrNotConfigured, http.StatusServiceUnavailable},
		{"stale delegation", ErrCredentialsRejected, http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, &fakeStore{err: tt.err}, http.MethodPost, "/api/groups/team@example.com/members", `{"email":"a@example.com"}`)
			if rec.Code != tt.want {
				t.Fatalf("got status %d, want %d; body %s", rec.Code, tt.want, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("got content-type %q, want application/problem+json", got)
			}
		})
	}
}

func decodeMembers(t *testing.T, rec *httptest.ResponseRecorder) ListMembersResponse {
	t.Helper()

	var got ListMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}

	return got
}

// The join is on email and Google is inconsistent about case, so a member whose address
// differs only in case from the stored user must still be matched.
func TestListMembersHandlerAttachesProfiles(t *testing.T) {
	store := &fakeStore{members: []Member{
		{Email: "Signed.In@Example.com", Role: RoleMember},
		{Email: "never-here@example.com", Role: RoleMember},
	}}
	profiles := &fakeProfiles{users: []user.User{
		{Email: "signed.in@example.com", Name: "Kai Chen", Nickname: "Kai", Department: "CSIE"},
	}}

	rec := serveWithProfiles(t, store, profiles, http.MethodGet, "/api/groups/team@example.com/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	got := decodeMembers(t, rec)
	if got.TotalItems != 2 {
		t.Fatalf("got %d members, want 2", got.TotalItems)
	}

	matched := got.Items[0]
	if matched.Profile == nil {
		t.Fatalf("expected a profile for %q", matched.Email)
	}
	if matched.Profile.Nickname != "Kai" || matched.Profile.Department != "CSIE" {
		t.Fatalf("unexpected profile: %+v", *matched.Profile)
	}

	// Someone on the mailing list who has never signed in has no local row at all, and
	// that is reported as a null profile rather than blank fields.
	if got.Items[1].Profile != nil {
		t.Fatalf("expected a null profile for %q, got %+v", got.Items[1].Email, *got.Items[1].Profile)
	}

	if len(profiles.gotEmails) != 2 {
		t.Fatalf("expected one lookup covering both members, got %v", profiles.gotEmails)
	}
}

// The roster came back from Google fine; a database problem should cost the caller the
// profile decoration, not the whole endpoint.
func TestListMembersHandlerDegradesWhenProfileLookupFails(t *testing.T) {
	store := &fakeStore{members: []Member{{Email: "someone@example.com", Role: RoleMember}}}
	profiles := &fakeProfiles{err: errors.New("database unavailable")}

	rec := serveWithProfiles(t, store, profiles, http.MethodGet, "/api/groups/team@example.com/members", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	got := decodeMembers(t, rec)
	if len(got.Items) != 1 || got.Items[0].Profile != nil {
		t.Fatalf("expected one member with a null profile, got %+v", got.Items)
	}
}

func decodeGroups(t *testing.T, rec *httptest.ResponseRecorder) ListGroupsResponse {
	t.Helper()

	var got ListGroupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}

	return got
}

// The list is what the frontend renders as the club's directory, so it carries the
// club's own names and classification, not just what Google happens to store.
func TestListGroupsHandlerAttachesChartMetadata(t *testing.T) {
	store := &fakeStore{groups: []Group{
		{Email: "core-system", Name: "NYCU SDC Core System"},
		{Email: "general", Name: "NYCU SDC General"},
	}}

	rec := serveWithProfiles(t, store, &fakeProfiles{}, http.MethodGet, "/api/groups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	got := decodeGroups(t, rec)

	// Organizational order: All Members comes before Project Team, so general leads
	// even though core-system sorts first alphabetically.
	if got.Items[0].Email != "general" || got.Items[1].Email != "core-system" {
		t.Fatalf("got order %q, %q; want organizational order", got.Items[0].Email, got.Items[1].Email)
	}

	if got.Items[0].Section.Key != "all" || got.Items[1].Section.Key != "project" {
		t.Fatalf("unexpected sections: %+v", []GroupSection{got.Items[0].Section, got.Items[1].Section})
	}
	if got.Items[1].DisplayName != "Core System" {
		t.Fatalf("got displayName %q, want Core System", got.Items[1].DisplayName)
	}
	// Google's own name is preserved alongside it.
	if got.Items[1].Name != "NYCU SDC Core System" {
		t.Fatalf("got name %q, want Google's name untouched", got.Items[1].Name)
	}
}

// A newly created mailing list nobody has classified yet must still appear, sorted
// last, rather than vanishing from the directory.
func TestListGroupsHandlerKeepsUnclassifiedGroups(t *testing.T) {
	store := &fakeStore{groups: []Group{
		{Email: "brand-new-group", Name: "Brand New"},
		{Email: "general", Name: "NYCU SDC General"},
	}}

	got := decodeGroups(t, serveWithProfiles(t, store, &fakeProfiles{}, http.MethodGet, "/api/groups", ""))

	last := got.Items[len(got.Items)-1]
	if last.Email != "brand-new-group" {
		t.Fatalf("got %q last, want the unclassified group", last.Email)
	}
	if last.Section.Key != orgchart.UnsectionedKey {
		t.Fatalf("got section %q, want %q", last.Section.Key, orgchart.UnsectionedKey)
	}
	if last.DisplayName != "brand-new-group" {
		t.Fatalf("got displayName %q, want the key as fallback", last.DisplayName)
	}
}

// Unlike the personal profile view, the account-wide directory reports hidden sections:
// omitting a row there would read as the group having gone missing.
func TestListGroupsHandlerReportsHiddenSections(t *testing.T) {
	store := &fakeStore{groups: []Group{{Email: "info", Name: "NYCU SDC Mail Backup"}}}

	got := decodeGroups(t, serveWithProfiles(t, store, &fakeProfiles{}, http.MethodGet, "/api/groups", ""))

	if got.TotalItems != 1 || got.Items[0].Section.Key != "system" {
		t.Fatalf("expected the system list to be reported, got %+v", got.Items)
	}
}
