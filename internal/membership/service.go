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
	"sort"
	"strings"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/orgchart"

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

// AddToLoginGroup puts email on the mailing list that gates sign-in, and reports the
// lists they are on afterwards along with the role that was actually applied.
//
// The role is validated here rather than by the caller because NormalizeRole lives in
// internal/googlegroup, which internal/user may not import. An empty role means MEMBER,
// matching Google's own default.
//
// Note what a non-MEMBER role means: auth.RoleResolver maps OWNER and MANAGER of the
// login group onto this service's admin role, so adding someone as either is granting
// them administrative access here.
func (s *Service) AddToLoginGroup(ctx context.Context, email, role string) ([]string, string, error) {
	traceCtx, span := s.tracer.Start(ctx, "AddToLoginGroup")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	normalized, err := googlegroup.NormalizeRole(role)
	if err != nil {
		return nil, "", err
	}

	if _, err := s.groups.AddMember(traceCtx, s.loginGroup, email, normalized); err != nil {
		span.RecordError(err)
		return nil, "", err
	}

	logger.Info("Added member to the login group",
		zap.String("login_group", s.loginGroup), zap.String("email", email), zap.String("role", normalized))

	keys, err := s.GroupKeysForEmail(traceCtx, email)
	if err != nil {
		span.RecordError(err)
		return nil, "", err
	}

	// The Directory API is eventually consistent, so the read above may not show the
	// membership that was just created. Reporting a list without the login group would
	// look exactly like the write having been undone, so fill in the one fact this
	// request knows for certain.
	return s.visibleKeys(withKey(keys, s.loginGroup)), normalized, nil
}

// RemoveFromLoginGroup takes email off the mailing list that gates sign-in, which also
// removes them from the roster and stops them signing in.
func (s *Service) RemoveFromLoginGroup(ctx context.Context, email string) error {
	traceCtx, span := s.tracer.Start(ctx, "RemoveFromLoginGroup")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if err := s.groups.RemoveMember(traceCtx, s.loginGroup, email); err != nil {
		span.RecordError(err)
		return err
	}

	logger.Info("Removed member from the login group",
		zap.String("login_group", s.loginGroup), zap.String("email", email))

	return nil
}

// withKey returns keys with want present, comparing the way group keys are compared
// everywhere else -- the login group may be configured as a full address while the
// lists come back bare.
func withKey(keys []string, want string) []string {
	bare := want
	if at := strings.Index(bare, "@"); at >= 0 {
		bare = bare[:at]
	}

	for _, k := range keys {
		if strings.EqualFold(k, bare) || strings.EqualFold(k, want) {
			return keys
		}
	}

	return append(append([]string(nil), keys...), bare)
}

// directIndex reads every group's member list once and inverts it into
// lowercased email -> the groups that person is directly on.
func (s *Service) directIndex(ctx context.Context) (map[string][]string, error) {
	all, err := s.groups.ListGroups(ctx)
	if err != nil {
		return nil, err
	}

	lists := make([][]googlegroup.Member, len(all))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(memberLookupConcurrency)
	for i, g := range all {
		group.Go(func() error {
			members, err := s.groups.ListMembers(groupCtx, g.Email)
			if err != nil {
				return err
			}
			lists[i] = members
			return nil
		})
	}
	// One unreadable group means the roster would quietly be missing people, which is
	// far harder to notice than an error, so the failure propagates.
	if err := group.Wait(); err != nil {
		return nil, err
	}

	index := make(map[string][]string)
	for i, g := range all {
		for _, m := range lists[i] {
			if m.Type != memberTypeUser {
				continue
			}
			email := strings.ToLower(m.Email)
			index[email] = append(index[email], g.Email)
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
