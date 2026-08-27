// Package membership answers "which mailing lists is this person on", expanded through
// the club's nested group structure and presented in organizational order.
//
// It is its own vertical slice rather than part of internal/user or internal/googlegroup
// because it needs both, plus internal/orgchart -- and internal/user is forbidden from
// importing internal/googlegroup (see CLAUDE.md). Nothing imports this package except
// main, so it can depend on all three freely.
package membership

import (
	"context"
	"errors"
	"sort"
	"strings"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/orgchart"
	"nycu-sdc/nexmin/internal/user"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// GroupSource is the consumer-side view of the Google mailing list service.
type GroupSource interface {
	ListGroupsForUser(ctx context.Context, email string) ([]googlegroup.Group, error)
	ListGroups(ctx context.Context) ([]googlegroup.Group, error)
	ListMembers(ctx context.Context, groupKey string) ([]googlegroup.Member, error)
	AddMember(ctx context.Context, groupKey, email, role string) (googlegroup.Member, error)
	RemoveMember(ctx context.Context, groupKey, memberKey string) error
}

// memberTypeUser is the Directory API's member type for a person, as opposed to
// "GROUP" for a nested mailing list.
const memberTypeUser = "USER"

// memberLookupConcurrency bounds how many group member lists are read at once when
// building the roster index. High enough that three dozen lists resolve in a handful of
// round trips, low enough not to fan out into Google's rate limiter.
const memberLookupConcurrency = 8

// Membership is one mailing list a person is on.
type Membership struct {
	Key         string
	Name        string
	MemberCount int64
}

// Section is a set of memberships under one heading from the org chart.
type Section struct {
	Key   string
	Name  string
	Items []Membership
}

// Result is everything the profile page needs about one person's mailing lists.
type Result struct {
	Sections []Section

	// Leadership reports whether the person is on a group that holds an officer role.
	//
	// It is deliberately a boolean and not the specific position: every office holder
	// is MANAGER in every department group, so Google cannot say which of the six
	// positions someone holds. See docs/organization.md section 4.
	Leadership bool
}

type Service struct {
	logger *zap.Logger
	tracer trace.Tracer

	groups     GroupSource
	chart      *orgchart.Chart
	loginGroup string
}

func NewService(logger *zap.Logger, groups GroupSource, chart *orgchart.Chart, loginGroup string) *Service {
	return &Service{
		logger:     logger,
		tracer:     otel.Tracer("membership/service"),
		groups:     groups,
		chart:      chart,
		loginGroup: loginGroup,
	}
}

// ForEmail returns every mailing list the address reaches, direct or nested, grouped
// into the org chart's sections.
func (s *Service) ForEmail(ctx context.Context, email string) (Result, error) {
	traceCtx, span := s.tracer.Start(ctx, "ForEmail")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	memberships, err := s.expand(traceCtx, email)
	if err != nil {
		span.RecordError(err)
		return Result{}, err
	}

	logger.Debug("Resolved mailing lists for user", zap.String("email", email), zap.Int("count", len(memberships)))

	return Result{
		Sections:   s.sectioned(memberships),
		Leadership: s.isLeadership(memberships),
	}, nil
}

// GroupKeysForEmail returns just the group keys, in organizational order.
//
// It exists for the compact list carried on GET /api/users/me, which only needs names
// and must not pay for the full sectioned shape.
func (s *Service) GroupKeysForEmail(ctx context.Context, email string) ([]string, error) {
	traceCtx, span := s.tracer.Start(ctx, "GroupKeysForEmail")
	defer span.End()

	memberships, err := s.expand(traceCtx, email)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	keys := make([]string, 0, len(memberships))
	for _, m := range memberships {
		keys = append(keys, m.Key)
	}

	return s.visibleKeys(keys), nil
}

// visibleKeys filters group keys down to the ones the profile views show and puts them
// in organizational order.
//
// Both shapes of the answer -- the per-user lookup and the roster index -- go through
// this, so they cannot disagree about which lists a person is on or in what order.
func (s *Service) visibleKeys(keys []string) []string {
	visible := make([]string, 0, len(keys))
	for _, key := range keys {
		if s.chart.SectionOf(key).Hidden {
			continue
		}
		visible = append(visible, key)
	}

	sort.Slice(visible, func(i, j int) bool { return s.less(visible[i], visible[j]) })

	return visible
}

// RosterGroupKeys returns, for every direct member of the login group, the mailing
// lists that person reaches -- keyed by lowercased email, in organizational order.
//
// It answers for the whole roster at once by inverting the question: instead of asking
// Google "which groups is this person on" once per person, it reads each group's member
// list and builds the reverse index. That is one call per group rather than one per
// person, and the club grows in people, not in mailing lists. The calls also go through
// the same member cache GET /api/groups/{group_key}/members uses, so a warm server
// answers without touching Google at all.
func (s *Service) RosterGroupKeys(ctx context.Context) (map[string][]string, error) {
	traceCtx, span := s.tracer.Start(ctx, "RosterGroupKeys")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	direct, err := s.directIndex(traceCtx)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// The login group defines the roster: it is the same list the sign-in gate reads,
	// so "on the roster" and "able to sign in" cannot drift apart.
	roster, err := s.groups.ListMembers(traceCtx, s.loginGroup)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	keys := make(map[string][]string, len(roster))
	for _, m := range roster {
		if m.Type != memberTypeUser {
			continue
		}

		email := strings.ToLower(m.Email)
		keys[email] = s.visibleKeys(direct[email])
	}

	logger.Debug("Built roster", zap.String("login_group", s.loginGroup), zap.Int("people", len(keys)))

	return keys, nil
}

// membershipWrite is one membership an add request will create, in the order it is
// written.
type membershipWrite struct {
	key  string
	role string
}

// AddToRoster puts email on the mailing list that gates sign-in and on every other list
// the caller named, and reports the lists they are on afterwards along with the role
// actually applied on the login group.
//
// The login group is always written and does not have to be named: being on it is what
// makes someone a member of the club, and callers should not have to know its address.
// Naming it anyway is how a role is set on it -- and note what a non-MEMBER role there
// means, since auth.RoleResolver maps OWNER and MANAGER of the login group onto this
// service's admin role.
//
// Roles and group keys are validated before anything is written, so a typo costs nothing
// rather than leaving the person on half the lists. Being on a list already is not a
// failure: the request says where they should end up, and that part of it is already
// true. Nothing is rolled back if a later write fails, but because every write is
// idempotent, repeating the whole request is always safe.
//
// The returned loginRole is empty when the login group membership already existed, which
// is the caller's signal that it has to look the current role up rather than derive it
// from what it asked for.
func (s *Service) AddToRoster(ctx context.Context, email string, groups []user.GroupRole) ([]string, string, error) {
	traceCtx, span := s.tracer.Start(ctx, "AddToRoster")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	writes, err := s.planWrites(traceCtx, groups)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}

	loginRole := ""
	for i, w := range writes {
		_, err := s.groups.AddMember(traceCtx, w.key, email, w.role)
		switch {
		case err == nil:
			if i == 0 {
				loginRole = w.role
			}
			logger.Info("Added member to a mailing list",
				zap.String("group_key", w.key), zap.String("email", email), zap.String("role", w.role))
		case errors.Is(err, googlegroup.ErrMemberAlreadyExists):
			logger.Info("Member is already on the mailing list, leaving them as they are",
				zap.String("group_key", w.key), zap.String("email", email))
		default:
			span.RecordError(err)
			return nil, "", err
		}
	}

	keys, err := s.GroupKeysForEmail(traceCtx, email)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}

	// The Directory API is eventually consistent, so the read above may not show the
	// memberships that were just created. Reporting a list without them would look
	// exactly like the write having been undone, so fill in what this request knows for
	// certain.
	want := make([]string, 0, len(writes))
	for _, w := range writes {
		want = append(want, w.key)
	}

	return s.visibleKeys(withKeys(keys, want)), loginRole, nil
}

// planWrites turns the requested lists into the ordered set of memberships to create,
// with the login group first, and rejects anything Google would.
//
// The login group leads so that a failure part-way through still leaves the person on the
// roster and able to sign in, where the problem is visible and the request can simply be
// repeated.
func (s *Service) planWrites(ctx context.Context, groups []user.GroupRole) ([]membershipWrite, error) {
	// Google's own default for members.insert, and what someone added without a role
	// should get.
	writes := []membershipWrite{{key: s.loginGroup, role: googlegroup.RoleMember}}
	loginNamed := false

	for _, g := range groups {
		key := strings.TrimSpace(g.Key)
		if key == "" {
			return nil, handlerutil.NewValidationError("groups", g.Key, "group key is required")
		}

		role, err := googlegroup.NormalizeRole(g.Role)
		if err != nil {
			return nil, err
		}

		if sameGroupKey(key, s.loginGroup) {
			if loginNamed {
				return nil, handlerutil.NewValidationError("groups", key, "group is listed more than once")
			}
			loginNamed = true
			writes[0].role = role
			continue
		}

		for _, w := range writes[1:] {
			if sameGroupKey(w.key, key) {
				return nil, handlerutil.NewValidationError("groups", key, "group is listed more than once")
			}
		}

		writes = append(writes, membershipWrite{key: key, role: role})
	}

	if err := s.validateGroupKeys(ctx, writes[1:]); err != nil {
		return nil, err
	}

	return writes, nil
}

// validateGroupKeys rejects keys that name no mailing list in the account.
//
// Checking up front is what keeps a typo from leaving someone on half the lists they were
// meant to be on. It reads the account-wide group list, which is cached, and is skipped
// entirely when the caller named no lists -- the login group is configuration, not caller
// input, and is not checked here.
func (s *Service) validateGroupKeys(ctx context.Context, writes []membershipWrite) error {
	if len(writes) == 0 {
		return nil
	}

	all, err := s.groups.ListGroups(ctx)
	if err != nil {
		return err
	}

	for _, w := range writes {
		if !knownGroup(all, w.key) {
			return handlerutil.NewValidationError("groups", w.key, "no mailing list with this key exists")
		}
	}

	return nil
}

// knownGroup reports whether key addresses one of the account's groups, by bare name,
// full address, alias or immutable ID -- every spelling the Directory API accepts.
func knownGroup(all []googlegroup.Group, key string) bool {
	for _, g := range all {
		if g.ID != "" && strings.EqualFold(g.ID, key) {
			return true
		}
		if sameGroupKey(g.Email, key) {
			return true
		}
		for _, alias := range g.Aliases {
			if sameGroupKey(alias, key) {
				return true
			}
		}
	}

	return false
}

// RemoveFromRoster takes email off every mailing list they are listed on.
//
// Removing them from the login group alone would take them off the roster and stop them
// signing in while leaving them on the lists that actually carry the club's mail. The
// login group goes last, so a failure part-way through leaves them on the roster where
// they are still visible and the request can simply be repeated.
//
// Nothing is rolled back on failure, but every removal is idempotent, so repeating the
// whole request is always safe.
func (s *Service) RemoveFromRoster(ctx context.Context, email string) error {
	traceCtx, span := s.tracer.Start(ctx, "RemoveFromRoster")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	// expand, deliberately, and not GroupKeysForEmail: that one drops the sections the
	// chart marks hidden, and a list nobody displays still delivers mail.
	lists, err := s.expand(traceCtx, email)
	if err != nil {
		span.RecordError(err)
		return err
	}

	for _, m := range lists {
		if sameGroupKey(m.Key, s.loginGroup) {
			continue
		}
		if err := s.removeMembership(traceCtx, logger, m.Key, email); err != nil {
			span.RecordError(err)
			return err
		}
	}

	// Always attempted, whether or not the read above listed it: someone added moments
	// ago may not show up on their own group list yet, and leaving them on the login
	// group would leave them able to sign in.
	if err := s.removeMembership(traceCtx, logger, s.loginGroup, email); err != nil {
		span.RecordError(err)
		return err
	}

	logger.Info("Removed member from every mailing list",
		zap.String("email", email), zap.Int("lists", len(lists)))

	return nil
}

// removeMembership takes email off one list, treating "they were not on it" as success.
//
// Google answers a member key it cannot resolve with a 400 whose detail reads "Missing
// required field: memberKey" rather than a 404, so both sentinels count. Either way the
// request asked for them not to be on the list, and they are not.
func (s *Service) removeMembership(ctx context.Context, logger *zap.Logger, groupKey, email string) error {
	err := s.groups.RemoveMember(ctx, groupKey, email)
	switch {
	case err == nil:
		logger.Info("Removed member from a mailing list",
			zap.String("group_key", groupKey), zap.String("email", email))
		return nil
	case errors.Is(err, googlegroup.ErrMemberNotFound), errors.Is(err, googlegroup.ErrInvalidMemberRequest):
		logger.Debug("Member was not on the mailing list, nothing to remove",
			zap.String("group_key", groupKey), zap.String("email", email))
		return nil
	default:
		return err
	}
}

// withKeys returns keys with every entry of want present, compared the way group keys are
// compared everywhere else -- the login group may be configured as a full address, and a
// caller may name a list either way, while the lists themselves come back bare.
func withKeys(keys, want []string) []string {
	out := append([]string(nil), keys...)

	for _, w := range want {
		bare := bareGroupKey(w)

		found := false
		for _, k := range out {
			if sameGroupKey(k, bare) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, bare)
		}
	}

	return out
}

// sameGroupKey reports whether two keys name the same mailing list. Only the part before
// the "@" is compared: the API speaks bare names, while configuration and Google both
// deal in full addresses.
func sameGroupKey(a, b string) bool {
	return strings.EqualFold(bareGroupKey(a), bareGroupKey(b))
}

func bareGroupKey(key string) string {
	if at := strings.Index(key, "@"); at >= 0 {
		return key[:at]
	}

	return key
}

// groupMembers is one group's member list, carrying the key it was read under.
//
// The key travels with the members rather than being looked up again by position when
// the index is assembled: the two are minutes apart in wall-clock terms on a cold cache,
// and pairing them afterwards means anything that reorders the group list in between
// attributes every one of these people to the wrong mailing list.
type groupMembers struct {
	key     string
	members []googlegroup.Member
}

// directIndex reads every group's member list once and inverts it into
// lowercased email -> the groups that person is directly on.
func (s *Service) directIndex(ctx context.Context) (map[string][]string, error) {
	all, err := s.groups.ListGroups(ctx)
	if err != nil {
		return nil, err
	}

	lists := make([]groupMembers, len(all))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(memberLookupConcurrency)
	for i, g := range all {
		key := g.Email
		group.Go(func() error {
			members, err := s.groups.ListMembers(groupCtx, key)
			if err != nil {
				return err
			}
			lists[i] = groupMembers{key: key, members: members}
			return nil
		})
	}
	// One unreadable group means the roster would quietly be missing people, which is
	// far harder to notice than an error, so the failure propagates.
	if err := group.Wait(); err != nil {
		return nil, err
	}

	index := make(map[string][]string)
	for _, list := range lists {
		for _, m := range list.members {
			if m.Type != memberTypeUser {
				continue
			}
			email := strings.ToLower(m.Email)
			index[email] = append(index[email], list.key)
		}
	}

	return index, nil
}

// expand returns the mailing lists the address is listed on, in organizational order.
//
// **Direct membership only.** The club's lists nest -- consultants sits inside
// committee, design inside branding -- but expanding that would say every consultant
// is on the committee, which is true of where their mail lands and false of where they
// sit in the club. This API answers the second question. See docs/organization.md.
func (s *Service) expand(ctx context.Context, email string) ([]Membership, error) {
	direct, err := s.groups.ListGroupsForUser(ctx, email)
	if err != nil {
		return nil, err
	}

	memberships := make([]Membership, 0, len(direct))
	seen := make(map[string]bool, len(direct))
	for _, g := range direct {
		if seen[g.Email] {
			continue
		}
		seen[g.Email] = true
		memberships = append(memberships, Membership{
			Key:         g.Email,
			Name:        s.chart.Name(g.Email),
			MemberCount: g.DirectMembersCount,
		})
	}

	sort.Slice(memberships, func(i, j int) bool {
		return s.less(memberships[i].Key, memberships[j].Key)
	})

	return memberships, nil
}

// less orders two group keys the way the org chart does, falling back to the key so
// unclassified lists stay in a stable order among themselves.
func (s *Service) less(a, b string) bool {
	oa, ob := s.chart.Order(a), s.chart.Order(b)
	if oa != ob {
		return oa < ob
	}
	return a < b
}

// sectioned buckets memberships under the org chart's sections, dropping sections the
// chart marks hidden and keeping the chart's ordering.
func (s *Service) sectioned(memberships []Membership) []Section {
	bySection := make(map[string][]Membership)
	meta := make(map[string]orgchart.Section)

	for _, m := range memberships {
		section := s.chart.SectionOf(m.Key)
		if section.Hidden {
			continue
		}
		bySection[section.Key] = append(bySection[section.Key], m)
		meta[section.Key] = section
	}

	sections := make([]Section, 0, len(bySection))
	for _, section := range s.chart.Sections() {
		items, ok := bySection[section.Key]
		if !ok {
			continue
		}
		sections = append(sections, Section{Key: section.Key, Name: section.Name, Items: items})
		delete(bySection, section.Key)
	}

	// Whatever is left is unsectioned: a mailing list nobody has classified yet. It
	// goes last rather than being dropped, so a new group is visible immediately.
	for key, items := range bySection {
		sections = append(sections, Section{Key: key, Name: meta[key].Name, Items: items})
	}

	return sections
}

func (s *Service) isLeadership(memberships []Membership) bool {
	leadership := make(map[string]bool)
	for _, key := range s.chart.LeadershipGroups() {
		leadership[key] = true
	}

	for _, m := range memberships {
		if leadership[m.Key] {
			return true
		}
	}

	return false
}
