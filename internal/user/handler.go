package user

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"nycu-sdc/nexmin/internal/jwt"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Store is the consumer-side view of the service this handler needs.
type Store interface {
	GetByID(ctx context.Context, id uuid.UUID) (User, error)
	ListByEmails(ctx context.Context, emails []string) ([]User, error)
	UpdateProfile(ctx context.Context, id uuid.UUID, name, nickname, department *string) (User, error)
	UpdateRole(ctx context.Context, id uuid.UUID, role string) (User, error)
}

// RoleResolver reports the role each email holds in the login mailing list right now,
// keyed by lowercased address. It is satisfied by auth.RoleResolver.
//
// The interface is declared here, in terms of plain strings, so that internal/user
// never imports internal/googlegroup -- internal/googlegroup imports this package to
// attach profiles to mailing list members, and the reverse edge would be a cycle.
type RoleResolver interface {
	LocalRoles(ctx context.Context) (map[string]string, error)

	// LocalRoleFor maps a login-group role onto this service's role without a Google
	// round trip, which is what makes it usable straight after a write.
	LocalRoleFor(groupRole string) string
}

// MembershipLister names the mailing lists an address is on, in organizational order.
// It is satisfied by membership.Service.
//
// Like RoleResolver it is declared in terms of plain strings so this package never
// imports internal/googlegroup.
// GroupRole names one mailing list and the role to grant on it.
//
// Plain strings, like the rest of this package's outward-facing contracts: the legal
// roles are Google's, and validating them lives in internal/googlegroup, which
// internal/user may not import.
type GroupRole struct {
	Key  string
	Role string
}

// RosterWriter changes which mailing lists someone is on, and therefore who is on the
// roster and who can sign in. Satisfied by membership.Service.
type RosterWriter interface {
	// AddToRoster puts email on the login mailing list and on every list named, and
	// reports the lists they are on afterwards.
	//
	// loginRole is the role actually applied on the login group, or "" when the
	// membership already existed and nothing was written -- in which case the caller has
	// to look the current role up rather than derive it from what it asked for.
	AddToRoster(ctx context.Context, email string, groups []GroupRole) (keys []string, loginRole string, err error)

	// RemoveFromRoster takes email off every mailing list they are on.
	RemoveFromRoster(ctx context.Context, email string) error
}

type MembershipLister interface {
	GroupKeysForEmail(ctx context.Context, email string) ([]string, error)

	// RosterGroupKeys answers for everyone on the login group at once, keyed by
	// lowercased email.
	RosterGroupKeys(ctx context.Context) (map[string][]string, error)
}

// UpdateProfileRequest is a partial update: a nil field is left as it is, so a client
// sending only {"nickname": "..."} does not blank out the rest. The pointers are what
// keep "field omitted" distinguishable from "field set to empty string", which is a
// meaningful request for nickname and department.
type UpdateProfileRequest struct {
	Name       *string `json:"name"`
	Nickname   *string `json:"nickname"`
	Department *string `json:"department"`
}

type Response struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Nickname   string    `json:"nickname"`
	Department string    `json:"department"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`

	// Groups is the compact list of mailing lists this user reaches, in organizational
	// order. Null -- not empty -- when Google could not be reached; the full shape,
	// with sections and nesting paths, is GET /api/users/me/groups.
	Groups []string `json:"groups"`
}

// GroupMembershipRequest is one mailing list to put the new member on, and the role to
// give them there. Roles are Google's MEMBER/MANAGER/OWNER, not this service's; omit for
// MEMBER, which is Google's own default.
type GroupMembershipRequest struct {
	Key  string `json:"key" validate:"required"`
	Role string `json:"role"`
}

// AddRosterMemberRequest adds someone to the club.
type AddRosterMemberRequest struct {
	Email string `json:"email" validate:"required,email"`

	// Groups are the mailing lists to put them on beyond the login group, which is
	// always written and does not have to be named. Naming it anyway is allowed and is
	// how a role is set on it -- MANAGER or OWNER there grants this service's admin.
	Groups []GroupMembershipRequest `json:"groups" validate:"dive"`
}

// RosterProfile is what this service knows about someone beyond their membership.
// It exists only once they have signed in at least once.
type RosterProfile struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Nickname   string    `json:"nickname"`
	Department string    `json:"department"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// RosterEntry is one person on the club roster.
//
// The roster comes from the login mailing list, not from this service's database: the
// list decides who exists, and the local row -- when there is one -- only supplies the
// profile they filled in here.
type RosterEntry struct {
	Email  string   `json:"email"`
	Role   string   `json:"role"`
	Groups []string `json:"groups"`

	// Profile is null for someone who is on the mailing list but has never signed in.
	Profile *RosterProfile `json:"profile"`
}

type RosterResponse struct {
	Items      []RosterEntry `json:"items"`
	TotalItems int           `json:"totalItems"`
}

type Handler struct {
	logger        *zap.Logger
	validator     *validator.Validate
	problemWriter *problem.HttpWriter
	tracer        trace.Tracer

	store       Store
	roles       RoleResolver
	memberships MembershipLister
	roster      RosterWriter
}

func NewHandler(logger *zap.Logger, validator *validator.Validate, problemWriter *problem.HttpWriter, store Store, roles RoleResolver, memberships MembershipLister, roster RosterWriter) *Handler {
	return &Handler{
		logger:        logger,
		validator:     validator,
		problemWriter: problemWriter,
		tracer:        otel.Tracer("user/handler"),
		store:         store,
		roles:         roles,
		memberships:   memberships,
		roster:        roster,
	}
}

// MeHandler returns the caller's own profile.
//
// The row is re-read from the database rather than serialized straight out of the
// request context: the access token carries only the subject, email and role claims, so
// the User the JWT middleware puts in the context has an empty Name and knows nothing
// about nickname or department.
func (h *Handler) MeHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "MeHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	id, ok := h.callerID(traceCtx, r, w, logger)
	if !ok {
		return
	}

	user, err := h.store.GetByID(traceCtx, id)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, h.withGroups(traceCtx, logger, h.respond(traceCtx, logger, user)))
}

// UpdateMeHandler edits the fields the caller owns: name, nickname and department.
//
// Role is deliberately absent. It is derived from the login mailing list and can only
// be changed there; letting it be PATCHed here would make the service its own authority.
func (h *Handler) UpdateMeHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "UpdateMeHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	id, ok := h.callerID(traceCtx, r, w, logger)
	if !ok {
		return
	}

	var req UpdateProfileRequest
	if err := handlerutil.ParseAndValidateRequestBody(traceCtx, h.validator, r, &req); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	name, nickname, department, err := normalizeRequest(req)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	user, err := h.store.UpdateProfile(traceCtx, id, name, nickname, department)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	logger.Info("User updated their profile", zap.String("user_id", user.ID.String()))

	handlerutil.WriteJSONResponse(w, http.StatusOK, h.withGroups(traceCtx, logger, h.respond(traceCtx, logger, user)))
}

// GetHandler returns one user by ID. Admin only; see the route table in main.go.
func (h *Handler) GetHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "GetHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	id, err := handlerutil.ParseUUID(r.PathValue("user_id"))
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	user, err := h.store.GetByID(traceCtx, id)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, h.respond(traceCtx, logger, user))
}

// ListHandler returns the club roster: every direct member of the login mailing list,
// with the lists each of them reaches and, for those who have signed in, their profile.
//
// The mailing list is the source of who exists; the users table is an extension of it,
// holding only what this service owns -- profile, UUID, and the cached role. So this is
// "read the login group, then fill in local profiles", not "list local rows".
//
// Admin only. Unlike GET /api/users/me there is nothing to degrade to when Google is
// unavailable: without the mailing list there is no roster, so the request fails.
func (h *Handler) ListHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "ListHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	groups, err := h.memberships.RosterGroupKeys(traceCtx)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	emails := make([]string, 0, len(groups))
	for email := range groups {
		emails = append(emails, email)
	}

	profiles, err := h.store.ListByEmails(traceCtx, emails)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}
	byEmail := make(map[string]User, len(profiles))
	for _, u := range profiles {
		byEmail[strings.ToLower(u.Email)] = u
	}

	roles, err := h.roles.LocalRoles(traceCtx)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	items := make([]RosterEntry, 0, len(groups))
	for email, keys := range groups {
		role, ok := roles[email]
		if !ok {
			// Being on the roster means being on the login group, so this should not
			// happen; treat it as no privileges rather than inventing one.
			role = RoleMember
		}

		entry := RosterEntry{Email: email, Role: role, Groups: keys}
		if u, ok := byEmail[email]; ok {
			entry.Email = u.Email
			entry.Profile = newRosterProfile(u)
		}
		items = append(items, entry)
	}

	// A roster is read by people, so sort by the name they go by, falling back to the
	// address for anyone who has not filled one in.
	sort.Slice(items, func(i, j int) bool {
		ki, kj := rosterSortKey(items[i]), rosterSortKey(items[j])
		if ki != kj {
			return ki < kj
		}
		return items[i].Email < items[j].Email
	})

	// Deliberately no role write-back here, unlike respondAll: the roster reports the
	// live value from the login group already, and refreshing the cached column would
	// mean writing a row per person on every list request.

	handlerutil.WriteJSONResponse(w, http.StatusOK, RosterResponse{
		Items:      items,
		TotalItems: len(items),
	})
}

// AddHandler puts someone on the club roster by adding them to the login mailing list,
// and to whatever other lists the request names. Admin only.
//
// The login group is what decides who exists and who may sign in, so this is the whole of
// "add a member" -- there is no local row to create; one appears when they first sign
// in. Note that naming the login group with a role of MANAGER or OWNER grants this
// service's admin role.
//
// Adding someone who is already on one of the lists is not an error: the request says
// where they should end up, and that part of it is already true.
func (h *Handler) AddHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "AddHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	var req AddRosterMemberRequest
	if err := handlerutil.ParseAndValidateRequestBody(traceCtx, h.validator, r, &req); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	// Converted rather than mapped field by field, the same way jwt.User is: the
	// conversion stops compiling if either shape drifts from the other.
	wanted := make([]GroupRole, 0, len(req.Groups))
	for _, g := range req.Groups {
		wanted = append(wanted, GroupRole(g))
	}

	groups, loginRole, err := h.roster.AddToRoster(traceCtx, req.Email, wanted)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	entry := RosterEntry{
		Email:  req.Email,
		Role:   h.addedRole(traceCtx, logger, req.Email, loginRole),
		Groups: groups,
	}

	// Someone added to the club may already have signed in before -- if they were
	// removed and are being restored, their profile is still here.
	if existing, err := h.store.ListByEmails(traceCtx, []string{req.Email}); err != nil {
		logger.Warn("Failed to load the profile of the added member", zap.String("email", req.Email), zap.Error(err))
	} else if len(existing) > 0 {
		entry.Email = existing[0].Email
		entry.Profile = newRosterProfile(existing[0])
	}

	logger.Info("Added member to the roster",
		zap.String("email", req.Email), zap.Int("groups", len(groups)), zap.String("login_role", loginRole))

	handlerutil.WriteJSONResponse(w, http.StatusCreated, entry)
}

// addedRole reports the local role of someone who was just added.
//
// loginRole is what the write applied on the login group, and mapping it locally is what
// keeps this correct straight after a write: Google may not have propagated the new
// membership yet, so reading it back could report nothing. An empty loginRole means the
// membership already existed and nothing was written, and then the live value is the only
// answer -- falling back to member, as everywhere else, when Google cannot be reached.
func (h *Handler) addedRole(ctx context.Context, logger *zap.Logger, email, loginRole string) string {
	if loginRole != "" {
		return h.roles.LocalRoleFor(loginRole)
	}

	roles, err := h.roles.LocalRoles(ctx)
	if err != nil {
		logger.Warn("Failed to resolve the role of an existing member, reporting member", zap.String("email", email), zap.Error(err))
		return RoleMember
	}

	if role, ok := roles[strings.ToLower(email)]; ok {
		return role
	}

	return RoleMember
}

// RemoveHandler takes someone off the club roster, and off every other mailing list they
// are on. Admin only.
//
// Removing them from the login group alone would take them off the roster and stop them
// signing in while leaving them on the lists that actually carry the club's mail, which
// is not what "remove from the club" means to anyone reading it.
//
// Addressed by email, not by the UUID GET /api/users/{user_id} takes: someone who has
// never signed in has no local row and therefore no UUID, and they are exactly the
// people a roster edit is most likely to touch.
//
// The local profile row is left alone, so it comes back if they are ever re-added.
//
// Nothing stops an admin removing themselves, which would leave them unable to sign
// back in. The equivalent group route has no such guard either, and one that only
// existed here would be trivially bypassed.
func (h *Handler) RemoveHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "RemoveHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	email := r.PathValue("email")
	if strings.TrimSpace(email) == "" {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.NewValidationError("email", email, "email is required"), logger)
		return
	}

	if err := h.roster.RemoveFromRoster(traceCtx, email); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	logger.Info("Removed member from every mailing list", zap.String("email", email))

	w.WriteHeader(http.StatusNoContent)
}

func newRosterProfile(u User) *RosterProfile {
	return &RosterProfile{
		ID:         u.ID.String(),
		Name:       u.Name,
		Nickname:   u.Nickname,
		Department: u.Department,
		CreatedAt:  u.CreatedAt.Time,
		UpdatedAt:  u.UpdatedAt.Time,
	}
}

func rosterSortKey(e RosterEntry) string {
	if e.Profile != nil && strings.TrimSpace(e.Profile.Name) != "" {
		return strings.ToLower(e.Profile.Name)
	}
	return strings.ToLower(e.Email)
}

// callerID pulls the authenticated user's ID out of the request context, writing the
// 401 itself when the JWT middleware did not run. ok is false once a response is written.
func (h *Handler) callerID(ctx context.Context, r *http.Request, w http.ResponseWriter, logger *zap.Logger) (uuid.UUID, bool) {
	caller, err := jwt.GetUserFromContext(ctx)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(ctx, r, w, handlerutil.ErrUnauthorized, logger)
		return uuid.UUID{}, false
	}

	return caller.ID, true
}

// withGroups attaches the caller's mailing lists to their own profile response.
//
// A failure leaves Groups nil and logs a warning rather than failing the request:
// Google credentials are optional by design, and the profile page must not stop loading
// because the Directory API is unreachable. Same principle as the role fallback in
// respondAll.
func (h *Handler) withGroups(ctx context.Context, logger *zap.Logger, res Response) Response {
	keys, err := h.memberships.GroupKeysForEmail(ctx, res.Email)
	if err != nil {
		logger.Warn("Failed to resolve mailing lists for user, omitting them", zap.String("email", res.Email), zap.Error(err))
		return res
	}

	res.Groups = keys
	return res
}

func (h *Handler) respond(ctx context.Context, logger *zap.Logger, user User) Response {
	return h.respondAll(ctx, logger, []User{user})[0]
}

// respondAll renders users with the role each one holds in the login mailing list right
// now, rather than the value stored on the row.
//
// users.role is a cache of the mailing list, written at sign-in. Because refreshing a
// session never re-reads the roster, that cache can be stale for as long as someone
// keeps refreshing, so read paths recompute it and write back any drift they find. The
// roster is fetched once per request and served from the googlegroup member cache, so
// this costs no Google call in the common case.
//
// This changes what is reported, not what is permitted: RequireRole still reads the
// role claim minted into the access token, which lags by at most one token lifetime.
func (h *Handler) respondAll(ctx context.Context, logger *zap.Logger, users []User) []Response {
	items := make([]Response, 0, len(users))

	roles, err := h.roles.LocalRoles(ctx)
	if err != nil {
		// Degrade to the stored role rather than failing the request. Google
		// credentials are optional by design, so an unconfigured or briefly unavailable
		// Directory API must not take the user endpoints down with it.
		logger.Warn("Failed to resolve roles from the login group, falling back to stored roles", zap.Error(err))
		roles = nil
	}

	for _, u := range users {
		if roles != nil {
			u = h.syncRole(ctx, logger, u, roles)
		}
		items = append(items, newResponse(u))
	}

	return items
}

// syncRole returns u with the role the login group currently grants, writing the new
// value back when it differs. An address that is no longer on the roster keeps a local
// account but loses every privilege, so it falls back to RoleMember rather than to
// whatever was last stored.
func (h *Handler) syncRole(ctx context.Context, logger *zap.Logger, u User, roles map[string]string) User {
	role, ok := roles[strings.ToLower(u.Email)]
	if !ok {
		role = RoleMember
	}

	if role == u.Role {
		return u
	}

	updated, err := h.store.UpdateRole(ctx, u.ID, role)
	if err != nil {
		// The response still reports the authoritative role; only the cache write lost.
		logger.Warn("Failed to write back the role resolved from the login group",
			zap.String("user_id", u.ID.String()), zap.Error(err))
		u.Role = role
		return u
	}

	logger.Info("Synced user role from the login group",
		zap.String("user_id", u.ID.String()),
		zap.String("email", u.Email),
		zap.String("from", u.Role),
		zap.String("to", role))

	return updated
}

// normalizeRequest trims and validates the fields that were supplied, leaving the
// others nil so the update stays partial.
func normalizeRequest(req UpdateProfileRequest) (name, nickname, department *string, err error) {
	if req.Name != nil {
		normalized, err := NormalizeName(*req.Name)
		if err != nil {
			return nil, nil, nil, err
		}
		name = &normalized
	}

	if req.Nickname != nil {
		normalized, err := NormalizeProfileField("nickname", *req.Nickname, MaxNicknameLength)
		if err != nil {
			return nil, nil, nil, err
		}
		nickname = &normalized
	}

	if req.Department != nil {
		normalized, err := NormalizeProfileField("department", *req.Department, MaxDepartmentLength)
		if err != nil {
			return nil, nil, nil, err
		}
		department = &normalized
	}

	return name, nickname, department, nil
}

func newResponse(u User) Response {
	return Response{
		ID:         u.ID.String(),
		Email:      u.Email,
		Name:       u.Name,
		Nickname:   u.Nickname,
		Department: u.Department,
		Role:       u.Role,
		CreatedAt:  u.CreatedAt.Time,
		UpdatedAt:  u.UpdatedAt.Time,
	}
}
