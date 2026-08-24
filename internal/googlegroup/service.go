package googlegroup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
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
		logger:      logger,
		tracer:      otel.Tracer("googlegroup/service"),
		memberCache: newCache[Member](ttl),
		groupCache:  newCache[Group](ttl),
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
		admin.AdminDirectoryGroupMemberReadonlyScope,
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
	traceCtx, span := s.tracer.Start(ctx, "ListMembers")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if !s.configured {
		return nil, ErrNotConfigured
	}

	if cached, ok := s.memberCache.get(groupKey); ok {
		logger.Debug("Serving mailing list members from cache", zap.String("group_key", groupKey), zap.Int("count", len(cached)))
		return cached, nil
	}

	logger.Info("Fetching mailing list members from Google", zap.String("group_key", groupKey))

	members := make([]Member, 0)
	err := s.svc.Members.List(groupKey).
		MaxResults(maxResultsPerPage).
		Pages(traceCtx, func(page *admin.Members) error {
			for _, m := range page.Members {
				members = append(members, Member{
					ID:     m.Id,
					Email:  m.Email,
					Role:   m.Role,
					Type:   m.Type,
					Status: m.Status,
				})
			}
			return nil
		})
	if err != nil {
		err = translateAPIError(err, groupKey)
		logger.Error("Failed to list mailing list members", zap.String("group_key", groupKey), zap.Error(err))
		span.RecordError(err)
		return nil, err
	}

	s.memberCache.set(groupKey, members)

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

	groups := make([]Group, 0)
	err := s.svc.Groups.List().
		Customer(customerMyCustomer).
		MaxResults(maxResultsPerPage).
		Pages(traceCtx, func(page *admin.Groups) error {
			for _, g := range page.Groups {
				groups = append(groups, Group{
					ID:                 g.Id,
					Email:              g.Email,
					Name:               g.Name,
					Description:        g.Description,
					DirectMembersCount: g.DirectMembersCount,
					AdminCreated:       g.AdminCreated,
					Aliases:            g.Aliases,
				})
			}
			return nil
		})
	if err != nil {
		err = translateAPIError(err, customerMyCustomer)
		logger.Error("Failed to list groups", zap.Error(err))
		span.RecordError(err)
		return nil, err
	}

	s.groupCache.set(allGroupsCacheKey, groups)

	logger.Info("Fetched group list", zap.Int("count", len(groups)))

	return groups, nil
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
