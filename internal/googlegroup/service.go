package googlegroup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/singleflight"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	// defaultCacheTTL is used when cache_ttl is unset or unparseable.
	defaultCacheTTL = 5 * time.Minute

	// maxResultsPerPage is the Directory API's maximum page size for members.list
	// and groups.list.
	maxResultsPerPage = 200

	// allGroupsCacheKey is the cache key for the organization-wide group list, which
	// unlike a member list has no natural key of its own.
	allGroupsCacheKey = "*"

	// customerMyCustomer resolves to the Workspace account that owns the impersonated
	// admin, so every domain in the account is covered.
	customerMyCustomer = "my_customer"
)

// The roles the Directory API accepts for a group member. They are also what the login
// gate reads to decide who administers the club through this API.
const (
	RoleMember  = "MEMBER"
	RoleManager = "MANAGER"
	RoleOwner   = "OWNER"
)

// NormalizeRole upper-cases a caller-supplied role and rejects anything the Directory
// API would not accept. An empty role means MEMBER, which is Google's own default.
//
// Validation lives here rather than in a `oneof` struct tag so that the set of legal
// roles is defined exactly once, next to the constants, and stays case-insensitive.
func NormalizeRole(role string) (string, error) {
	switch upper := strings.ToUpper(strings.TrimSpace(role)); upper {
	case "":
		return RoleMember, nil
	case RoleMember, RoleManager, RoleOwner:
		return upper, nil
	default:
		return "", handlerutil.NewValidationError("role", role, "role must be one of MEMBER, MANAGER, OWNER")
	}
}

// Member is a single mailing list member as returned by the Admin SDK.
type Member struct {
	ID     string
	Email  string
	Role   string
	Type   string
	Status string
}

// Group is a single Google group in the organization.
type Group struct {
	ID                 string
	Email              string
	Name               string
	Description        string
	DirectMembersCount int64
	AdminCreated       bool
	Aliases            []string
}

type Service struct {
	logger *zap.Logger
	tracer trace.Tracer

	svc         *admin.Service
	memberCache *cache[Member]
	groupCache  *cache[Group]

	// memberFlight collapses concurrent reads of the same group's members into one
	// call. Without it, every request arriving while an entry is expired issues its
	// own -- exactly the amplification the cache exists to prevent, and the roster
	// makes it matter: it reads every group's member list at once, so an expiry would
	// otherwise be multiplied by both the group count and the number of callers.
	//
	// It is replaced wholesale on invalidation rather than held by value: singleflight
	// hands every joiner the in-flight call's result, so a request arriving after a
	// write would otherwise be served the pre-write list it was waiting on. Swapping
	// the group means requests that arrive after the write start their own call.
	memberFlightMu sync.Mutex
	memberFlight   *singleflight.Group

	// domain is the Workspace domain the club's own groups live in, without the "@".
	// Empty disables the bare-name handling entirely.
	domain string

	// configured reports whether a service account key was supplied. When false the
	// service is inert and every call returns ErrNotConfigured, so the process can
	// still start and serve unrelated routes without Google credentials.
	configured bool
}

// NewService builds a Directory API client authenticated as a service account using
// domain-wide delegation.
//
// If cfg.ServiceAccountKey is empty the service is returned unconfigured rather than
// failing: Google credentials are only needed by this one feature and requiring them
// would block all local development.
func NewService(logger *zap.Logger, cfg Config) (*Service, error) {
	ttl := defaultCacheTTL
	if cfg.CacheTTL != "" {
		parsed, err := time.ParseDuration(cfg.CacheTTL)
		if err != nil {
			return nil, fmt.Errorf("invalid google_group.cache_ttl %q: %w", cfg.CacheTTL, err)
		}
		ttl = parsed
	}

	s := &Service{
		logger:       logger,
		tracer:       otel.Tracer("googlegroup/service"),
		memberCache:  newCache[Member](ttl),
		groupCache:   newCache[Group](ttl),
		memberFlight: &singleflight.Group{},
		// Tolerate "@sdc.nycu.club" as well as "sdc.nycu.club": both readings of
		// "domain" are natural and neither is worth a startup failure.
		domain: strings.TrimPrefix(strings.TrimSpace(cfg.Domain), "@"),
	}

	if cfg.ServiceAccountKey == "" {
		logger.Warn("Google service account is not configured, mailing list endpoints will be unavailable")
		return s, nil
	}

	keyJSON, err := base64.StdEncoding.DecodeString(cfg.ServiceAccountKey)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode google service account key: %w", err)
	}

	if cfg.ImpersonateSubject == "" {
		// The Admin SDK will reject the token at call time with an opaque 400, so fail
		// here where the cause is obvious.
		return nil, errors.New("google_group.impersonate_subject is required when a service account key is set")
	}

	// Changing this scope list requires re-granting domain-wide delegation in the
	// Workspace admin console with the full set. Delegation grants are matched against
	// the exact scope list, not merged, so a stale grant makes every call fail with
	// unauthorized_client -- including ones that worked before the change.
	jwtConfig, err := google.JWTConfigFromJSON(keyJSON,
		// Read-write superset of ...MemberReadonlyScope: members are added, re-roled and
		// removed through this service.
		admin.AdminDirectoryGroupMemberScope,
		// Group listing stays read-only; nothing here creates or edits groups themselves.
		admin.AdminDirectoryGroupReadonlyScope,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse google service account key: %w", err)
	}

	// Domain-wide delegation: act on behalf of a real Workspace admin.
	jwtConfig.Subject = cfg.ImpersonateSubject

	ctx := context.Background()
	adminService, err := admin.NewService(ctx, option.WithHTTPClient(jwtConfig.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("failed to create google admin directory service: %w", err)
	}

	s.svc = adminService
	s.configured = true

	logger.Info("Google group service initialized", zap.String("impersonate_subject", cfg.ImpersonateSubject), zap.Duration("cache_ttl", ttl))

	return s, nil
}

// ListMembers returns every member of groupKey, which may be the group's email
// address or its immutable Google group ID. Results are cached for the configured TTL.
func (s *Service) ListMembers(ctx context.Context, groupKey string) ([]Member, error) {
	if !s.configured {
		return nil, ErrNotConfigured
	}

	return withGroupKey(s, groupKey, func(key string) ([]Member, error) {
		return s.listMembers(ctx, key)
	})
}

func (s *Service) listMembers(ctx context.Context, groupKey string) ([]Member, error) {
	traceCtx, span := s.tracer.Start(ctx, "ListMembers")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if cached, ok := s.memberCache.get(groupKey); ok {
		logger.Debug("Serving mailing list members from cache", zap.String("group_key", groupKey), zap.Int("count", len(cached)))
		return cached, nil
	}

	members, err, _ := s.flight().Do(groupKey, func() (any, error) {
		return s.fetchMembers(traceCtx, logger, groupKey)
	})
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	return members.([]Member), nil
}

func (s *Service) fetchMembers(ctx context.Context, logger *zap.Logger, groupKey string) ([]Member, error) {
	// Re-check under the flight: the winner of a race populates the cache, and the
	// callers queued behind it should read that rather than fetch again.
	if cached, ok := s.memberCache.get(groupKey); ok {
		return cached, nil
	}

	logger.Info("Fetching mailing list members from Google", zap.String("group_key", groupKey))

	// Taken before the fetch: if a write invalidates the cache while this is in
	// flight, set discards the result rather than resurrecting the pre-write list.
	gen := s.memberCache.begin()

	members := make([]Member, 0)
	err := s.svc.Members.List(groupKey).
		MaxResults(maxResultsPerPage).
		Pages(ctx, func(page *admin.Members) error {
			for _, m := range page.Members {
				members = append(members, toMember(m))
			}
			return nil
		})
	if err != nil {
		err = translateAPIError(err, groupKey)
		logger.Error("Failed to list mailing list members", zap.String("group_key", groupKey), zap.Error(err))
		return nil, err
	}

	s.memberCache.set(groupKey, gen, members)

	logger.Info("Fetched mailing list members", zap.String("group_key", groupKey), zap.Int("count", len(members)))

	return members, nil
}

// ListGroups returns every group in the Workspace account. Results are cached for the
// configured TTL.
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	traceCtx, span := s.tracer.Start(ctx, "ListGroups")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if !s.configured {
		return nil, ErrNotConfigured
	}

	if cached, ok := s.groupCache.get(allGroupsCacheKey); ok {
		logger.Debug("Serving group list from cache", zap.Int("count", len(cached)))
		return cached, nil
	}

	logger.Info("Fetching group list from Google")

	gen := s.groupCache.begin()

	groups := make([]Group, 0)
	err := s.svc.Groups.List().
		Customer(customerMyCustomer).
		MaxResults(maxResultsPerPage).
		Pages(traceCtx, func(page *admin.Groups) error {
			for _, g := range page.Groups {
				groups = append(groups, s.toGroup(g))
			}
			return nil
		})
	if err != nil {
		err = translateAPIError(err, customerMyCustomer)
		logger.Error("Failed to list groups", zap.Error(err))
		span.RecordError(err)
		return nil, err
	}

	s.groupCache.set(allGroupsCacheKey, gen, groups)

	logger.Info("Fetched group list", zap.Int("count", len(groups)))

	return groups, nil
}

// ListGroupsForUser returns the groups email belongs to.
//
// This is a separate call path rather than an argument to ListGroups because the
// Directory API refuses userKey and customer together: one asks "which groups does this
// person belong to", the other "which groups exist in the account".
//
// Results are not cached. Unlike the account-wide group list, this answer is per person,
// so a shared TTL cache would either hold one entry per member or thrash.
func (s *Service) ListGroupsForUser(ctx context.Context, email string) ([]Group, error) {
	traceCtx, span := s.tracer.Start(ctx, "ListGroupsForUser")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if !s.configured {
		return nil, ErrNotConfigured
	}

	groups := make([]Group, 0)
	err := s.svc.Groups.List().
		UserKey(email).
		MaxResults(maxResultsPerPage).
		Pages(traceCtx, func(page *admin.Groups) error {
			for _, g := range page.Groups {
				groups = append(groups, s.toGroup(g))
			}
			return nil
		})
	if err != nil {
		err = translateAPIError(err, email)
		logger.Error("Failed to list groups for user", zap.String("email", email), zap.Error(err))
		span.RecordError(err)
		return nil, err
	}

	logger.Info("Fetched groups for user", zap.String("email", email), zap.Int("count", len(groups)))

	return groups, nil
}

// AddMember adds email to groupKey with the given role and returns the created
// membership. Role must already have been through NormalizeRole.
func (s *Service) AddMember(ctx context.Context, groupKey, email, role string) (Member, error) {
	if !s.configured {
		return Member{}, ErrNotConfigured
	}

	return withGroupKey(s, groupKey, func(key string) (Member, error) {
		return s.addMember(ctx, key, email, role)
	})
}

func (s *Service) addMember(ctx context.Context, groupKey, email, role string) (Member, error) {
	traceCtx, span := s.tracer.Start(ctx, "AddMember")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	created, err := s.svc.Members.Insert(groupKey, &admin.Member{Email: email, Role: role}).Context(traceCtx).Do()
	if err != nil {
		err = translateMemberAPIError(err, groupKey, email)
		logger.Error("Failed to add mailing list member", zap.String("group_key", groupKey), zap.String("email", email), zap.Error(err))
		span.RecordError(err)
		return Member{}, err
	}

	s.invalidateCaches()

	logger.Info("Added mailing list member", zap.String("group_key", groupKey), zap.String("email", email), zap.String("role", created.Role))

	return toMember(created), nil
}

// UpdateMemberRole changes an existing member's role and returns the updated membership.
// memberKey may be the member's email address or their immutable Google member ID.
func (s *Service) UpdateMemberRole(ctx context.Context, groupKey, memberKey, role string) (Member, error) {
	if !s.configured {
		return Member{}, ErrNotConfigured
	}

	return withGroupKey(s, groupKey, func(key string) (Member, error) {
		return s.updateMemberRole(ctx, key, memberKey, role)
	})
}

func (s *Service) updateMemberRole(ctx context.Context, groupKey, memberKey, role string) (Member, error) {
	traceCtx, span := s.tracer.Start(ctx, "UpdateMemberRole")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	updated, err := s.svc.Members.Patch(groupKey, memberKey, &admin.Member{Role: role}).Context(traceCtx).Do()
	if err != nil {
		err = translateMemberAPIError(err, groupKey, memberKey)
		logger.Error("Failed to update mailing list member", zap.String("group_key", groupKey), zap.String("member_key", memberKey), zap.Error(err))
		span.RecordError(err)
		return Member{}, err
	}

	s.invalidateCaches()

	logger.Info("Updated mailing list member", zap.String("group_key", groupKey), zap.String("member_key", memberKey), zap.String("role", updated.Role))

	return toMember(updated), nil
}

// RemoveMember removes a member from groupKey. memberKey may be the member's email
// address or their immutable Google member ID.
func (s *Service) RemoveMember(ctx context.Context, groupKey, memberKey string) error {
	if !s.configured {
		return ErrNotConfigured
	}

	_, err := withGroupKey(s, groupKey, func(key string) (struct{}, error) {
		return struct{}{}, s.removeMember(ctx, key, memberKey)
	})

	return err
}

func (s *Service) removeMember(ctx context.Context, groupKey, memberKey string) error {
	traceCtx, span := s.tracer.Start(ctx, "RemoveMember")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if err := s.svc.Members.Delete(groupKey, memberKey).Context(traceCtx).Do(); err != nil {
		err = translateMemberAPIError(err, groupKey, memberKey)
		logger.Error("Failed to remove mailing list member", zap.String("group_key", groupKey), zap.String("member_key", memberKey), zap.Error(err))
		span.RecordError(err)
		return err
	}

	s.invalidateCaches()

	logger.Info("Removed mailing list member", zap.String("group_key", groupKey), zap.String("member_key", memberKey))

	return nil
}

// invalidateCaches drops both caches after a successful write.
//
// The group cache goes too because DirectMembersCount moves with every membership
// change. Dropping the member cache also means the login gate -- which reads through it
// -- sees a new member immediately instead of waiting out the TTL.
func (s *Service) invalidateCaches() {
	s.memberCache.clear()
	s.groupCache.clear()

	// Reads already waiting on a pre-write fetch cannot be rescued, but reads that
	// arrive from here on must not be joined to it.
	s.memberFlightMu.Lock()
	s.memberFlight = &singleflight.Group{}
	s.memberFlightMu.Unlock()
}

// flight returns the current member-read flight group. See the field's comment.
func (s *Service) flight() *singleflight.Group {
	s.memberFlightMu.Lock()
	defer s.memberFlightMu.Unlock()

	return s.memberFlight
}

func toMember(m *admin.Member) Member {
	return Member{
		ID:     m.Id,
		Email:  m.Email,
		Role:   m.Role,
		Type:   m.Type,
		Status: m.Status,
	}
}

// translateMemberAPIError maps errors from member-scoped calls. It differs from
// translateAPIError on 404 -- which here can mean either the group or the member, and
// the message says so rather than guessing -- and adds the conflict and bad-request
// cases that only writes can produce.
func translateMemberAPIError(err error, groupKey, memberKey string) error {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return translateAPIError(err, groupKey)
	}

	switch apiErr.Code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s in %s", ErrMemberNotFound, memberKey, groupKey)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s in %s", ErrMemberAlreadyExists, memberKey, groupKey)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrInvalidMemberRequest, apiErr.Message)
	default:
		return translateAPIError(err, groupKey)
	}
}

// translateAPIError maps Google API transport errors onto package sentinels so the
// central problem writer can turn them into meaningful status codes instead of 500s.
func translateAPIError(err error, groupKey string) error {
	// A token-endpoint failure means our own credentials or domain-wide delegation are
	// wrong -- an operator problem, not a caller problem -- so surface it as
	// unavailable rather than an opaque 500.
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return fmt.Errorf("%w: %s", ErrCredentialsRejected, retrieveErr.ErrorCode)
	}

	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return err
	}

	switch apiErr.Code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrGroupNotFound, groupKey)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrInsufficientPermission, groupKey)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrQuotaExceeded, groupKey)
	default:
		return err
	}
}

// The club runs a single Workspace domain, so the API speaks bare group names:
// "general" rather than "general@sdc.nycu.club". Addresses are qualified on the way
// into Google and shortened on the way back out, and google_group.domain is what
// enables it -- leave that empty and everything below is a no-op.

// qualifyGroupKey attaches the configured domain to a bare group name. A key that
// already carries an "@" is somebody's full address and is left exactly as given.
func (s *Service) qualifyGroupKey(groupKey string) string {
	if s.domain == "" || groupKey == "" || strings.Contains(groupKey, "@") {
		return groupKey
	}

	return groupKey + "@" + s.domain
}

// shortGroupKey strips the configured domain back off a group address.
//
// Only an exact match is stripped. Group listing covers every domain in the Workspace
// account, so a group that lives somewhere else keeps its full address rather than
// being silently renamed into a key that would not resolve on the way back.
func (s *Service) shortGroupKey(email string) string {
	if s.domain == "" {
		return email
	}

	if bare, found := strings.CutSuffix(email, "@"+s.domain); found {
		return bare
	}

	return email
}

func (s *Service) shortGroupKeys(emails []string) []string {
	if s.domain == "" || len(emails) == 0 {
		return emails
	}

	short := make([]string, 0, len(emails))
	for _, email := range emails {
		short = append(short, s.shortGroupKey(email))
	}

	return short
}

// withGroupKey runs do against the qualified group key, retrying once with the key
// exactly as the caller supplied it when Google reports nothing found.
//
// A bare name and a group's immutable ID are indistinguishable up front -- both are
// letters and digits with no "@" -- so the ambiguity can only be settled by asking.
// Qualifying first keeps the common case to a single call, and the retry is what keeps
// addressing a group by its immutable ID working. Retrying a write is safe because the
// only outcome that triggers it is a 404, which means nothing was created or deleted.
func withGroupKey[T any](s *Service, groupKey string, do func(key string) (T, error)) (T, error) {
	qualified := s.qualifyGroupKey(groupKey)

	result, err := do(qualified)
	if err != nil && qualified != groupKey && isNotFound(err) {
		return do(groupKey)
	}

	return result, err
}

// isNotFound reports whether err is Google saying the addressed thing does not exist.
// Member operations report a bad group key as ErrMemberNotFound, since a 404 from
// members.insert cannot distinguish the two, so both sentinels count.
func isNotFound(err error) bool {
	return errors.Is(err, ErrGroupNotFound) || errors.Is(err, ErrMemberNotFound)
}

// toGroup maps the Admin SDK's group onto this package's, shortening the addresses so
// callers only ever see the bare names the API speaks.
func (s *Service) toGroup(g *admin.Group) Group {
	return Group{
		ID:                 g.Id,
		Email:              s.shortGroupKey(g.Email),
		Name:               g.Name,
		Description:        g.Description,
		DirectMembersCount: g.DirectMembersCount,
		AdminCreated:       g.AdminCreated,
		Aliases:            s.shortGroupKeys(g.Aliases),
	}
}
