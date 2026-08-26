package membership

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"nycu-sdc/club-manager/internal/googlegroup"
	"nycu-sdc/club-manager/internal/orgchart"

	"go.uber.org/zap/zaptest"
)

// fakeGroups replays a canned account shape, so the expansion can be tested without a
// Google client. It also counts lookups, since avoiding redundant ones is the whole
// point of walking lazily.
type fakeGroups struct {
	direct map[string][]string

	// members is the account seen from the other side: group key -> the addresses on
	// it. A "+" prefix marks a nested group rather than a person.
	members map[string][]string

	directErr  error
	listErr    error
	membersErr error
	writeErr   error

	added   [][3]string
	removed [][2]string

	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeGroups) ListGroupsForUser(_ context.Context, email string) ([]googlegroup.Group, error) {
	if f.directErr != nil {
		return nil, f.directErr
	}

	groups := make([]googlegroup.Group, 0)
	for _, key := range f.direct[email] {
		groups = append(groups, googlegroup.Group{Email: key, DirectMembersCount: 7})
	}
	return groups, nil
}

func (f *fakeGroups) AddMember(_ context.Context, groupKey, email, role string) (googlegroup.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeErr != nil {
		return googlegroup.Member{}, f.writeErr
	}
	f.added = append(f.added, [3]string{groupKey, email, role})
	return googlegroup.Member{Email: email, Role: role, Type: "USER"}, nil
}

func (f *fakeGroups) RemoveMember(_ context.Context, groupKey, memberKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeErr != nil {
		return f.writeErr
	}
	f.removed = append(f.removed, [2]string{groupKey, memberKey})
	return nil
}

func (f *fakeGroups) ListGroups(_ context.Context) ([]googlegroup.Group, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	groups := make([]googlegroup.Group, 0, len(f.members))
	for key := range f.members {
		groups = append(groups, googlegroup.Group{Email: key, DirectMembersCount: 7})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Email < groups[j].Email })
	return groups, nil
}

func (f *fakeGroups) ListMembers(_ context.Context, groupKey string) ([]googlegroup.Member, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls["members:"+groupKey]++
	f.mu.Unlock()

	if f.membersErr != nil {
		return nil, f.membersErr
	}

	out := make([]googlegroup.Member, 0)
	for _, e := range f.members[groupKey] {
		if strings.HasPrefix(e, "+") {
			out = append(out, googlegroup.Member{Email: strings.TrimPrefix(e, "+"), Type: "GROUP"})
			continue
		}
		out = append(out, googlegroup.Member{Email: e, Type: "USER"})
	}
	return out, nil
}

func newService(t *testing.T, groups GroupSource) *Service {
	t.Helper()

	chart, err := orgchart.Load()
	if err != nil {
		t.Fatalf("chart: %v", err)
	}
	return NewService(zaptest.NewLogger(t), groups, chart, "general")
}

// The real shape: design sits under branding, branding under committee, committee under
// general. Somebody only on design must reach all four.
func clubShape() *fakeGroups {
	return &fakeGroups{
		direct:  map[string][]string{"designer@example.com": {"design"}},
		members: map[string][]string{"design": {"designer@example.com"}},
	}
}

func TestExpandSortsIntoOrganizationalOrder(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"someone@example.com": {"hpc", "general", "engineering"}},
	}

	got, err := newService(t, groups).expand(t.Context(), "someone@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	keys := []string{got[0].Key, got[1].Key, got[2].Key}
	want := []string{"general", "engineering", "hpc"}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got order %v, want %v", keys, want)
		}
	}
}

func TestForEmailBucketsIntoSectionsAndHidesSystem(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"someone@example.com": {"engineering", "info", "general"}},
	}

	got, err := newService(t, groups).ForEmail(t.Context(), "someone@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, s := range got.Sections {
		if s.Key == "system" {
			t.Fatal("the hidden system section must not be returned")
		}
	}

	keys := make([]string, 0, len(got.Sections))
	for _, s := range got.Sections {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "all,departments" {
		t.Fatalf("got sections %v, want [all departments]", keys)
	}
}

// A group the chart has never heard of must surface, not vanish.
func TestForEmailKeepsUnsectionedGroupsLast(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"someone@example.com": {"general", "brand-new-group"}},
	}

	got, err := newService(t, groups).ForEmail(t.Context(), "someone@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	last := got.Sections[len(got.Sections)-1]
	if last.Key != orgchart.UnsectionedKey {
		t.Fatalf("got last section %q, want %q", last.Key, orgchart.UnsectionedKey)
	}
	if last.Items[0].Key != "brand-new-group" || last.Items[0].Name != "brand-new-group" {
		t.Fatalf("unexpected item: %+v", last.Items[0])
	}
}

func TestLeadershipFollowsPresidentsMembership(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{
			"officer@example.com": {"presidents"},
			"member@example.com":  {"engineering"},
		},
	}
	s := newService(t, groups)

	officer, err := s.ForEmail(t.Context(), "officer@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !officer.Leadership {
		t.Fatal("a member of presidents holds an officer role")
	}

	member, err := s.ForEmail(t.Context(), "member@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if member.Leadership {
		t.Fatal("being in a department is not an officer role")
	}
}

func TestGroupKeysForEmail(t *testing.T) {
	keys, err := newService(t, clubShape()).GroupKeysForEmail(t.Context(), "designer@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Direct membership only: design is where they are listed, and its parents are
	// deliberately not walked.
	if strings.Join(keys, ",") != "design" {
		t.Fatalf("got %v, want only the list they are actually on", keys)
	}
}

func TestErrorsPropagate(t *testing.T) {
	sentinel := errors.New("google unavailable")

	groups := clubShape()
	groups.directErr = sentinel
	if _, err := newService(t, groups).ForEmail(t.Context(), "designer@example.com"); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the lookup failure", err)
	}

	groups = clubShape()
	groups.membersErr = sentinel
	if _, err := newService(t, groups).RosterGroupKeys(t.Context()); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the member lookup failure", err)
	}
}

// The compact list and the sectioned view are two shapes of one answer; a list hidden
// from one must be hidden from the other.
func TestGroupKeysForEmailHidesTheSameSections(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"someone@example.com": {"general", "info"}},
	}

	keys, err := newService(t, groups).GroupKeysForEmail(t.Context(), "someone@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(keys, ",") != "general" {
		t.Fatalf("got %v, want the hidden system list excluded", keys)
	}
}

// rosterShape is the same club as clubShape, seen from the group side: general holds
// two people plus the nested committee, and design holds someone who is not on general.
func rosterShape() *fakeGroups {
	return &fakeGroups{
		members: map[string][]string{
			"general":   {"alice@example.com", "bob@example.com", "+committee"},
			"committee": {"alice@example.com"},
			"branding":  {"+design"},
			"design":    {"carol@example.com", "bob@example.com"},
		},
	}
}

// The roster is the login group, so someone who is only on design does not appear --
// they cannot sign in either, and the two definitions have to stay the same.
func TestRosterGroupKeysCoversOnlyTheLoginGroup(t *testing.T) {
	got, err := newService(t, rosterShape()).RosterGroupKeys(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d people, want 2: %v", len(got), got)
	}
	if _, ok := got["carol@example.com"]; ok {
		t.Fatal("someone outside the login group must not be on the roster")
	}
}

// A nested group is a member of the login group, but it is not a person.
func TestRosterGroupKeysSkipsNestedGroups(t *testing.T) {
	got, err := newService(t, rosterShape()).RosterGroupKeys(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := got["committee"]; ok {
		t.Fatal("a nested group must not be counted as a person")
	}
}

// The roster's answer is built by inverting the member lists, the per-user answer by
// asking Google about one person. They must agree.
func TestRosterGroupKeysAgreesWithGroupKeysForEmail(t *testing.T) {
	groups := rosterShape()
	// bob is directly on general and design, so his expansion reaches branding and
	// committee through design.
	groups.direct = map[string][]string{"bob@example.com": {"general", "design"}}

	s := newService(t, groups)

	roster, err := s.RosterGroupKeys(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	perUser, err := s.GroupKeysForEmail(t.Context(), "bob@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Join(roster["bob@example.com"], ",") != strings.Join(perUser, ",") {
		t.Fatalf("roster %v != per-user %v", roster["bob@example.com"], perUser)
	}
}

// One unreadable member list would silently drop people from the roster, which is much
// harder to notice than a failed request.
func TestRosterGroupKeysFailsOnUnreadableGroup(t *testing.T) {
	sentinel := errors.New("google unavailable")
	groups := rosterShape()
	groups.membersErr = sentinel

	if _, err := newService(t, groups).RosterGroupKeys(t.Context()); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the failure to propagate", err)
	}
}

// The club's lists nest -- design sits inside branding, consultants inside committee --
// and this service deliberately does not expand that. Expanding would report every
// consultant as being on the committee, which is true of where their mail lands and
// false of where they sit in the club.
//
// This test exists to stop the expansion being reintroduced: the tests that used to
// cover the walk are gone, so without it nothing would notice.
func TestExpandDoesNotWalkNesting(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"designer@example.com": {"design"}},
		// branding contains design, and committee contains branding.
		members: map[string][]string{
			"design":    {"designer@example.com"},
			"branding":  {"+design"},
			"committee": {"+branding"},
		},
	}

	got, err := newService(t, groups).GroupKeysForEmail(t.Context(), "designer@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Join(got, ",") != "design" {
		t.Fatalf("got %v, want only design -- nesting must not be expanded", got)
	}
}

// The roster answers for everyone at once, so it has to hold the same line.
func TestRosterGroupKeysDoesNotWalkNesting(t *testing.T) {
	groups := &fakeGroups{
		members: map[string][]string{
			"general":  {"designer@example.com"},
			"design":   {"designer@example.com"},
			"branding": {"+design"},
		},
	}

	got, err := newService(t, groups).RosterGroupKeys(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Join(got["designer@example.com"], ",") != "general,design" {
		t.Fatalf("got %v, want only the lists they are listed on", got["designer@example.com"])
	}
}

// Adding to the roster means adding to the login group -- that is the whole operation,
// and it must use the configured list rather than a hard-coded name.
func TestAddToLoginGroupTargetsTheConfiguredList(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}}}

	if _, _, err := newService(t, groups).AddToLoginGroup(t.Context(), "alice@example.com", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(groups.added) != 1 {
		t.Fatalf("got %d writes, want 1", len(groups.added))
	}
	if got := groups.added[0]; got[0] != "general" || got[1] != "alice@example.com" || got[2] != "MEMBER" {
		t.Fatalf("got %v, want the login group and Google's default role", got)
	}
}

func TestAddToLoginGroupNormalizesRole(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "MEMBER"},
		{"manager", "MANAGER"},
		{"OWNER", "OWNER"},
	}

	for _, tt := range tests {
		groups := &fakeGroups{members: map[string][]string{"general": {}}}
		_, role, err := newService(t, groups).AddToLoginGroup(t.Context(), "alice@example.com", tt.in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tt.in, err)
		}
		if role != tt.want {
			t.Fatalf("AddToLoginGroup(%q) applied %q, want %q", tt.in, role, tt.want)
		}
	}
}

func TestAddToLoginGroupRejectsUnknownRole(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}}}

	if _, _, err := newService(t, groups).AddToLoginGroup(t.Context(), "alice@example.com", "SUPERUSER"); err == nil {
		t.Fatal("expected an unknown role to be rejected")
	}
	if len(groups.added) != 0 {
		t.Fatal("a rejected role must not reach Google")
	}
}

// The Directory API is eventually consistent, so the read-back after the write can
// still be missing the membership that was just created. Reporting a list without the
// login group would look exactly like the write having been undone.
func TestAddToLoginGroupReportsTheLoginGroupEvenIfTheReadBackLags(t *testing.T) {
	groups := &fakeGroups{
		// direct is what ListGroupsForUser answers: deliberately stale, as if Google
		// has not propagated the write.
		direct:  map[string][]string{"alice@example.com": {}},
		members: map[string][]string{"general": {}},
	}

	keys, _, err := newService(t, groups).AddToLoginGroup(t.Context(), "alice@example.com", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(keys, ",") != "general" {
		t.Fatalf("got %v, want the login group present despite the lagging read", keys)
	}
}

// ...and it must not be listed twice once the read-back does catch up.
func TestAddToLoginGroupDoesNotDuplicateTheLoginGroup(t *testing.T) {
	groups := &fakeGroups{
		direct:  map[string][]string{"alice@example.com": {"general"}},
		members: map[string][]string{"general": {}},
	}

	keys, _, err := newService(t, groups).AddToLoginGroup(t.Context(), "alice@example.com", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(keys, ",") != "general" {
		t.Fatalf("got %v, want a single entry", keys)
	}
}

func TestRemoveFromLoginGroupTargetsTheConfiguredList(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}}}

	if err := newService(t, groups).RemoveFromLoginGroup(t.Context(), "alice@example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups.removed) != 1 || groups.removed[0] != [2]string{"general", "alice@example.com"} {
		t.Fatalf("got %v", groups.removed)
	}
}
