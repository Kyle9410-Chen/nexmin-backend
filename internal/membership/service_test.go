package membership

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/orgchart"
	"nycu-sdc/nexmin/internal/user"

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

	// groupList, when set, is what ListGroups hands back -- the same slice every time,
	// so a test can reorder it the way GET /api/groups used to reorder the cached one.
	groupList []googlegroup.Group

	// gate, when set, holds every member read until it is closed. entered receives once
	// per read that has arrived, so a test can wait until the fan-out is in flight.
	gate    chan struct{}
	entered chan struct{}

	directErr  error
	listErr    error
	membersErr error
	writeErr   error

	// writeErrFor fails one group's write, which is how a partial failure part-way
	// through a multi-list request is set up.
	writeErrFor map[string]error

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
	if err := f.writeErrFor[groupKey]; err != nil {
		return googlegroup.Member{}, err
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
	if err := f.writeErrFor[groupKey]; err != nil {
		return err
	}
	f.removed = append(f.removed, [2]string{groupKey, memberKey})
	return nil
}

// addedGroups is the list of groups written to, in the order they were written.
func (f *fakeGroups) addedGroups() []string {
	keys := make([]string, 0, len(f.added))
	for _, a := range f.added {
		keys = append(keys, a[0])
	}
	return keys
}

// removedGroups is the list of groups removed from, in the order they were removed.
func (f *fakeGroups) removedGroups() []string {
	keys := make([]string, 0, len(f.removed))
	for _, r := range f.removed {
		keys = append(keys, r[0])
	}
	return keys
}

func (f *fakeGroups) ListGroups(_ context.Context) ([]googlegroup.Group, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	if f.groupList != nil {
		return f.groupList, nil
	}

	groups := make([]googlegroup.Group, 0, len(f.members))
	for key := range f.members {
		groups = append(groups, googlegroup.Group{Email: key, DirectMembersCount: 7})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Email < groups[j].Email })
	return groups, nil
}

func (f *fakeGroups) ListMembers(_ context.Context, groupKey string) ([]googlegroup.Member, error) {
	if f.gate != nil {
		f.entered <- struct{}{}
		<-f.gate
	}

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

// The club is defined by the login group, so adding someone always writes it -- the
// caller does not have to know its address, and must not be able to leave it out.
func TestAddToRosterAlwaysWritesTheLoginGroup(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}}}

	_, role, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(groups.added) != 1 {
		t.Fatalf("got %d writes, want 1: %v", len(groups.added), groups.added)
	}
	if got := groups.added[0]; got[0] != "general" || got[1] != "alice@example.com" || got[2] != "MEMBER" {
		t.Fatalf("got %v, want the login group and Google's default role", got)
	}
	if role != "MEMBER" {
		t.Fatalf("got applied role %q, want MEMBER", role)
	}
}

// The login group goes first so that a failure further down still leaves the person on
// the roster, where the problem is visible and the request can be repeated.
func TestAddToRosterWritesEveryRequestedListWithTheLoginGroupFirst(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}, "engineering": {}, "design": {}}}

	_, _, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
		{Key: "engineering", Role: "manager"},
		{Key: "design"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(groups.addedGroups(), ","); got != "general,engineering,design" {
		t.Fatalf("got write order %q, want general,engineering,design", got)
	}
	if groups.added[1][2] != "MANAGER" {
		t.Fatalf("got %v, want the normalized role on engineering", groups.added[1])
	}
	if groups.added[2][2] != "MEMBER" {
		t.Fatalf("got %v, want Google's default role on design", groups.added[2])
	}
}

// Naming the login group is allowed, and is how a role is set on it -- which is also how
// admin is granted. It must still only be written once.
func TestAddToRosterHonoursAnExplicitLoginGroupRole(t *testing.T) {
	groups := &fakeGroups{members: map[string][]string{"general": {}}}

	_, role, err := newService(t, groups).AddToRoster(t.Context(), "officer@example.com", []user.GroupRole{
		{Key: "general", Role: "MANAGER"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(groups.added) != 1 {
		t.Fatalf("got %d writes, want 1: %v", len(groups.added), groups.added)
	}
	if groups.added[0][2] != "MANAGER" || role != "MANAGER" {
		t.Fatalf("got %v / %q, want MANAGER applied to the login group", groups.added[0], role)
	}
}

func TestAddToRosterNormalizesRole(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "MEMBER"},
		{"manager", "MANAGER"},
		{"OWNER", "OWNER"},
	}

	for _, tt := range tests {
		groups := &fakeGroups{members: map[string][]string{"general": {}}}
		_, role, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
			{Key: "general", Role: tt.in},
		})
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tt.in, err)
		}
		if role != tt.want {
			t.Fatalf("AddToRoster(%q) applied %q, want %q", tt.in, role, tt.want)
		}
	}
}

// The request says where someone should end up. Being on a list already is that request
// already being satisfied, not a failure -- and without this, adding an existing member
// to one more list would stop at the login group and do nothing at all.
func TestAddToRosterTreatsAnExistingMembershipAsSuccess(t *testing.T) {
	groups := &fakeGroups{
		members:     map[string][]string{"general": {}, "engineering": {}},
		writeErrFor: map[string]error{"general": googlegroup.ErrMemberAlreadyExists},
	}

	_, role, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
		{Key: "engineering"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(groups.addedGroups(), ","); got != "engineering" {
		t.Fatalf("got writes %q, want the remaining list to still be written", got)
	}
	// Nothing was written to the login group, so the caller has to look the role up.
	if role != "" {
		t.Fatalf("got applied role %q, want empty for a membership that already existed", role)
	}
}

// A typo must cost nothing rather than leaving someone on half the lists they were meant
// to be on, so everything is validated before anything is written.
func TestAddToRosterRejectsBadRequestsBeforeWritingAnything(t *testing.T) {
	tests := []struct {
		name string
		want []user.GroupRole
	}{
		{"unknown group", []user.GroupRole{{Key: "no-such-list"}}},
		{"unknown role", []user.GroupRole{{Key: "engineering", Role: "SUPERUSER"}}},
		{"empty key", []user.GroupRole{{Key: "  "}}},
		{"duplicate group", []user.GroupRole{{Key: "engineering"}, {Key: "engineering"}}},
		{"login group twice", []user.GroupRole{{Key: "general"}, {Key: "general@sdc.nycu.club"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := &fakeGroups{members: map[string][]string{"general": {}, "engineering": {}}}

			if _, _, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", tt.want); err == nil {
				t.Fatal("expected the request to be rejected")
			}
			if len(groups.added) != 0 {
				t.Fatalf("a rejected request reached Google: %v", groups.added)
			}
		})
	}
}

// Nothing is rolled back when a later list fails. That is deliberate -- every write is
// idempotent, so repeating the request is the recovery -- and it is pinned here so the
// behaviour is a decision rather than an accident.
func TestAddToRosterKeepsEarlierWritesWhenALaterOneFails(t *testing.T) {
	sentinel := errors.New("google unavailable")
	groups := &fakeGroups{
		members:     map[string][]string{"general": {}, "engineering": {}},
		writeErrFor: map[string]error{"engineering": sentinel},
	}

	if _, _, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
		{Key: "engineering"},
	}); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the failure to propagate", err)
	}

	if got := strings.Join(groups.addedGroups(), ","); got != "general" {
		t.Fatalf("got writes %q, want the login group left in place", got)
	}
}

// The Directory API is eventually consistent, so the read-back after the write can still
// be missing the memberships that were just created. Reporting a list without them would
// look exactly like the write having been undone.
func TestAddToRosterReportsEveryRequestedListEvenIfTheReadBackLags(t *testing.T) {
	groups := &fakeGroups{
		// direct is what ListGroupsForUser answers: deliberately stale, as if Google has
		// not propagated the writes.
		direct:  map[string][]string{"alice@example.com": {}},
		members: map[string][]string{"general": {}, "engineering": {}},
	}

	keys, _, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
		{Key: "engineering"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Join(keys, ",") != "general,engineering" {
		t.Fatalf("got %v, want both lists present despite the lagging read", keys)
	}
}

// ...and they must not be listed twice once the read-back does catch up.
func TestAddToRosterDoesNotDuplicateLists(t *testing.T) {
	groups := &fakeGroups{
		direct:  map[string][]string{"alice@example.com": {"general", "engineering"}},
		members: map[string][]string{"general": {}, "engineering": {}},
	}

	keys, _, err := newService(t, groups).AddToRoster(t.Context(), "alice@example.com", []user.GroupRole{
		{Key: "engineering"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Join(keys, ",") != "general,engineering" {
		t.Fatalf("got %v, want one entry each", keys)
	}
}

// Leaving someone on the lists that carry the club's mail is not "removing them from the
// club", so every list goes -- and the login group goes last, so a failure part-way
// through leaves them on the roster where they are still visible.
func TestRemoveFromRosterRemovesEveryListWithTheLoginGroupLast(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"leaver@example.com": {"general", "engineering", "design"}},
	}

	if err := newService(t, groups).RemoveFromRoster(t.Context(), "leaver@example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Organizational order for the rest -- expand already sorts them -- and the login
	// group last.
	if got := strings.Join(groups.removedGroups(), ","); got != "design,engineering,general" {
		t.Fatalf("got removal order %q, want the login group last", got)
	}
}

// The hidden system lists are dropped from every view, but they still deliver mail, so
// removal must not go through visibleKeys.
func TestRemoveFromRosterRemovesHiddenLists(t *testing.T) {
	groups := &fakeGroups{
		direct: map[string][]string{"leaver@example.com": {"general", "info"}},
	}

	if err := newService(t, groups).RemoveFromRoster(t.Context(), "leaver@example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(groups.removedGroups(), ","); got != "info,general" {
		t.Fatalf("got %q, want the hidden list removed too", got)
	}
}

// Someone added moments ago may not show up on their own group list yet, and leaving them
// on the login group would leave them able to sign in.
func TestRemoveFromRosterAlwaysRemovesTheLoginGroup(t *testing.T) {
	groups := &fakeGroups{direct: map[string][]string{"leaver@example.com": {}}}

	if err := newService(t, groups).RemoveFromRoster(t.Context(), "leaver@example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(groups.removed) != 1 || groups.removed[0] != [2]string{"general", "leaver@example.com"} {
		t.Fatalf("got %v, want the login group removed anyway", groups.removed)
	}
}

// Removing someone who is not on a list asks for a state that already holds. Google says
// so with a 400 rather than a 404, so both spellings have to count.
func TestRemoveFromRosterTreatsAMissingMembershipAsSuccess(t *testing.T) {
	for _, sentinel := range []error{googlegroup.ErrMemberNotFound, googlegroup.ErrInvalidMemberRequest} {
		groups := &fakeGroups{
			direct:      map[string][]string{"ghost@example.com": {"general", "engineering"}},
			writeErrFor: map[string]error{"general": sentinel, "engineering": sentinel},
		}

		if err := newService(t, groups).RemoveFromRoster(t.Context(), "ghost@example.com"); err != nil {
			t.Fatalf("got %v, want %v to be treated as success", err, sentinel)
		}
	}
}

func TestRemoveFromRosterPropagatesRealFailures(t *testing.T) {
	sentinel := errors.New("google unavailable")
	groups := &fakeGroups{
		direct:      map[string][]string{"leaver@example.com": {"general", "engineering"}},
		writeErrFor: map[string]error{"engineering": sentinel},
	}

	if err := newService(t, groups).RemoveFromRoster(t.Context(), "leaver@example.com"); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the failure to propagate", err)
	}
	if len(groups.removed) != 0 {
		t.Fatalf("the login group must not be removed after a failure: %v", groups.removed)
	}
}

// The group list and the member lists are read seconds apart when the cache is cold, and
// the group list is shared -- GET /api/groups sorts it into organizational order. Pairing
// a group with its members by position after the fact therefore attributed everyone to
// whichever group had moved into that slot, which is how people ended up on committee
// without being on it.
func TestRosterGroupKeysSurvivesTheGroupListBeingReordered(t *testing.T) {
	shared := []googlegroup.Group{{Email: "committee"}, {Email: "design"}, {Email: "general"}}

	groups := &fakeGroups{
		members: map[string][]string{
			"general":   {"alice@example.com", "bob@example.com"},
			"committee": {"alice@example.com"},
			"design":    {"bob@example.com"},
		},
		groupList: shared,
		gate:      make(chan struct{}),
		entered:   make(chan struct{}, 8),
	}

	type result struct {
		keys map[string][]string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		keys, err := newService(t, groups).RosterGroupKeys(context.Background())
		done <- result{keys, err}
	}()

	// Wait until every member read is in flight, then reorder the list underneath them.
	for range shared {
		<-groups.entered
	}
	sort.Slice(shared, func(i, j int) bool { return shared[i].Email > shared[j].Email })
	close(groups.gate)

	got := <-done
	if got.err != nil {
		t.Fatalf("unexpected error: %v", got.err)
	}

	if strings.Join(got.keys["alice@example.com"], ",") != "general,committee" {
		t.Fatalf("alice: got %v, want [general committee]", got.keys["alice@example.com"])
	}
	if strings.Join(got.keys["bob@example.com"], ",") != "general,design" {
		t.Fatalf("bob: got %v, want [general design]", got.keys["bob@example.com"])
	}
}
