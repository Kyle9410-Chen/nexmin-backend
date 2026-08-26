package auth

import (
	"context"
	"strings"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/user"

	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// activeMemberStatus is the Directory API status for a usable membership. Suspended or
// archived members must not be able to sign in.
//
// The API only populates status for identities that live inside the Workspace account;
// members from an outside domain -- which is most of the club -- come back with an
// empty string. Treating empty as inactive locks out everyone except @sdc.nycu.club
// addresses, so usableMemberStatus accepts it and rejects only the states Google names
// explicitly.
const activeMemberStatus = "ACTIVE"

// usableMemberStatus reports whether a membership status permits sign-in.
func usableMemberStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "", activeMemberStatus:
		return true
	default:
		return false
	}
}

// MemberChecker resolves the members of the group that gates login.
type MemberChecker interface {
	ListMembers(ctx context.Context, groupKey string) ([]googlegroup.Member, error)
}

// RoleResolver maps membership in the login mailing list onto this service's roles.
//
// It lives in internal/auth rather than internal/user for two reasons. The mapping is
// the same authority decision the login gate makes, so keeping one implementation stops
// the sign-in path and the read paths from disagreeing about who is an admin. And it
// breaks an import cycle: internal/googlegroup imports internal/user to attach profiles
// to mailing list members, so internal/user must not import internal/googlegroup back.
// internal/auth already depends on both, so the mapping belongs here and reaches
// internal/user through a small consumer-side interface on that side.
type RoleResolver struct {
	logger *zap.Logger
	tracer trace.Tracer

	members    MemberChecker
	loginGroup string
}

func NewRoleResolver(logger *zap.Logger, members MemberChecker, loginGroup string) *RoleResolver {
	return &RoleResolver{
		logger:     logger,
		tracer:     otel.Tracer("auth/roleresolver"),
		members:    members,
		loginGroup: loginGroup,
	}
}

// LoginGroup is the mailing list this resolver reads. An empty value means login is
// unconfigured, which refuses everyone rather than falling open.
func (r *RoleResolver) LoginGroup() string {
	return r.loginGroup
}

// RoleFor returns the caller's local role in the login group. found reports whether
// they hold a usable membership at all -- the login gate itself -- so one pass over the
// roster answers both "may they sign in" and "what may they do".
func (r *RoleResolver) RoleFor(ctx context.Context, email string) (role string, found bool, err error) {
	traceCtx, span := r.tracer.Start(ctx, "RoleFor")
	defer span.End()

	members, err := r.members.ListMembers(traceCtx, r.loginGroup)
	if err != nil {
		span.RecordError(err)
		return "", false, err
	}

	for _, m := range members {
		if strings.EqualFold(m.Email, email) && usableMemberStatus(m.Status) {
			return localRoleFor(m.Role), true, nil
		}
	}

	return "", false, nil
}

// LocalRoles returns a lowercased email -> local role map covering every usable member
// of the login group.
//
// Read paths use it to report the role someone holds right now instead of the possibly
// stale value cached in users.role. It is one roster fetch per request regardless of how
// many users are being rendered, and that fetch goes through the googlegroup member
// cache (google_group.cache_ttl, default 5m), so it rarely reaches Google at all.
func (r *RoleResolver) LocalRoles(ctx context.Context) (map[string]string, error) {
	traceCtx, span := r.tracer.Start(ctx, "LocalRoles")
	defer span.End()
	logger := logutil.WithContext(traceCtx, r.logger)

	members, err := r.members.ListMembers(traceCtx, r.loginGroup)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	roles := make(map[string]string, len(members))
	for _, m := range members {
		if !usableMemberStatus(m.Status) {
			continue
		}
		roles[strings.ToLower(m.Email)] = localRoleFor(m.Role)
	}

	logger.Debug("Resolved local roles from the login group",
		zap.String("login_group", r.loginGroup),
		zap.Int("members", len(roles)))

	return roles, nil
}

// LocalRoleFor maps a role in the login group onto this service's role, without asking
// Google anything. Callers that have just written a role and cannot trust a read-back
// yet use this rather than LocalRoles.
func (r *RoleResolver) LocalRoleFor(groupRole string) string {
	return localRoleFor(groupRole)
}

// localRoleFor maps a member's role in the login group onto this service's role.
//
// Owners and managers of the mailing list are already the people who administer club
// membership, so they are the ones allowed to change it through this API. Authority is
// never configured locally -- promoting someone in Google Groups is the whole procedure.
func localRoleFor(groupRole string) string {
	switch strings.ToUpper(groupRole) {
	case googlegroup.RoleOwner, googlegroup.RoleManager:
		return user.RoleAdmin
	default:
		return user.RoleMember
	}
}
