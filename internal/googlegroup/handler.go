package googlegroup

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"nycu-sdc/nexmin/internal/orgchart"
	"nycu-sdc/nexmin/internal/user"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/go-playground/validator/v10"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Store is the consumer-side view of the service this handler needs.
type Store interface {
	ListMembers(ctx context.Context, groupKey string) ([]Member, error)
	ListGroups(ctx context.Context) ([]Group, error)
	AddMember(ctx context.Context, groupKey, email, role string) (Member, error)
	UpdateMemberRole(ctx context.Context, groupKey, memberKey, role string) (Member, error)
	RemoveMember(ctx context.Context, groupKey, memberKey string) error
}

// ProfileStore resolves the local profiles of mailing list members.
type ProfileStore interface {
	ListByEmails(ctx context.Context, emails []string) ([]user.User, error)
}

// MemberProfile is what this service knows about a member beyond their address, filled
// in by the member themselves through PATCH /api/users/me.
type MemberProfile struct {
	Name       string `json:"name"`
	Nickname   string `json:"nickname"`
	Department string `json:"department"`
}

type MemberResponse struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Type   string `json:"type"`
	Status string `json:"status"`

	// Profile is null when the address has never signed in here, which is different
	// from a member who signed in and left the fields blank. Keeping it nested rather
	// than flattening the three fields onto the member is what preserves that
	// distinction for the frontend.
	//
	// Role above stays Google's MEMBER/MANAGER/OWNER; this service's own admin/member
	// role is derived from it, so reporting both would be the same fact twice.
	Profile *MemberProfile `json:"profile"`
}

type ListMembersResponse struct {
	Items      []MemberResponse `json:"items"`
	TotalItems int              `json:"totalItems"`
}

// Role is validated by NormalizeRole rather than a `oneof` tag so that the legal set is
// defined once, beside the constants, and comparison stays case-insensitive.
type AddMemberRequest struct {
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role"`
}

type UpdateMemberRequest struct {
	Role string `json:"role" validate:"required"`
}

// Chart is the display metadata this handler reads. Satisfied by *orgchart.Chart.
type Chart interface {
	Name(key string) string
	SectionOf(key string) orgchart.Section
	Order(key string) int
}

// GroupSection is where a mailing list sits in the club's structure.
type GroupSection struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type GroupResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// DisplayName is the club's own name for this list. Name above stays whatever is
	// set in the Google admin console, which carries a "NYCU SDC" prefix on every row.
	DisplayName string `json:"displayName"`

	// Section is the club's classification. A list the org chart does not mention
	// reports the synthetic "unsectioned" section rather than being hidden -- a newly
	// created mailing list has to be visible before anyone can classify it.
	Section GroupSection `json:"section"`

	DirectMembersCount int64    `json:"directMembersCount"`
	AdminCreated       bool     `json:"adminCreated"`
	Aliases            []string `json:"aliases"`
}

type ListGroupsResponse struct {
	Items      []GroupResponse `json:"items"`
	TotalItems int             `json:"totalItems"`
}

type Handler struct {
	logger        *zap.Logger
	validator     *validator.Validate
	problemWriter *problem.HttpWriter
	tracer        trace.Tracer

	store    Store
	profiles ProfileStore
	chart    Chart
}

func NewHandler(logger *zap.Logger, validator *validator.Validate, problemWriter *problem.HttpWriter, store Store, profiles ProfileStore, chart Chart) *Handler {
	return &Handler{
		logger:        logger,
		validator:     validator,
		problemWriter: problemWriter,
		tracer:        otel.Tracer("googlegroup/handler"),
		store:         store,
		profiles:      profiles,
		chart:         chart,
	}
}

func (h *Handler) ListMembersHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "ListMembersHandler")
	defer span.End()
	logger := h.logger.With(zap.String("handler", "ListMembersHandler"))

	groupKey := r.PathValue("group_key")
	if groupKey == "" {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.NewValidationError("group_key", groupKey, "group key is required"), logger)
		return
	}

	members, err := h.store.ListMembers(traceCtx, groupKey)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	emails := make([]string, 0, len(members))
	for _, m := range members {
		emails = append(emails, m.Email)
	}

	profiles := h.lookupProfiles(traceCtx, logger, emails)

	items := make([]MemberResponse, 0, len(members))
	for _, m := range members {
		items = append(items, newMemberResponse(m, profiles))
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, ListMembersResponse{
		Items:      items,
		TotalItems: len(items),
	})
}

func (h *Handler) AddMemberHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "AddMemberHandler")
	defer span.End()
	logger := h.logger.With(zap.String("handler", "AddMemberHandler"))

	groupKey := r.PathValue("group_key")
	if groupKey == "" {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.NewValidationError("group_key", groupKey, "group key is required"), logger)
		return
	}

	var req AddMemberRequest
	if err := handlerutil.ParseAndValidateRequestBody(traceCtx, h.validator, r, &req); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	role, err := NormalizeRole(req.Role)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	member, err := h.store.AddMember(traceCtx, groupKey, req.Email, role)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	handlerutil.WriteJSONResponse(w, http.StatusCreated, newMemberResponse(member, h.lookupProfiles(traceCtx, logger, []string{member.Email})))
}

func (h *Handler) UpdateMemberHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "UpdateMemberHandler")
	defer span.End()
	logger := h.logger.With(zap.String("handler", "UpdateMemberHandler"))

	groupKey, memberKey, ok := h.memberPath(traceCtx, r, w, logger)
	if !ok {
		return
	}

	var req UpdateMemberRequest
	if err := handlerutil.ParseAndValidateRequestBody(traceCtx, h.validator, r, &req); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	role, err := NormalizeRole(req.Role)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	member, err := h.store.UpdateMemberRole(traceCtx, groupKey, memberKey, role)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, newMemberResponse(member, h.lookupProfiles(traceCtx, logger, []string{member.Email})))
}

func (h *Handler) RemoveMemberHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "RemoveMemberHandler")
	defer span.End()
	logger := h.logger.With(zap.String("handler", "RemoveMemberHandler"))

	groupKey, memberKey, ok := h.memberPath(traceCtx, r, w, logger)
	if !ok {
		return
	}

	if err := h.store.RemoveMember(traceCtx, groupKey, memberKey); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	// Written directly rather than through WriteJSONResponse, which would marshal a
	// `null` body onto a 204.
	w.WriteHeader(http.StatusNoContent)
}

// memberPath pulls both path wildcards, writing the validation problem itself when one
// is missing. ok is false once a response has been written.
func (h *Handler) memberPath(ctx context.Context, r *http.Request, w http.ResponseWriter, logger *zap.Logger) (groupKey, memberKey string, ok bool) {
	groupKey = r.PathValue("group_key")
	if groupKey == "" {
		h.problemWriter.WriteErrorWithRequest(ctx, r, w, handlerutil.NewValidationError("group_key", groupKey, "group key is required"), logger)
		return "", "", false
	}

	memberKey = r.PathValue("member_key")
	if memberKey == "" {
		h.problemWriter.WriteErrorWithRequest(ctx, r, w, handlerutil.NewValidationError("member_key", memberKey, "member key is required"), logger)
		return "", "", false
	}

	return groupKey, memberKey, true
}

// lookupProfiles maps lowercased addresses to the local profile behind them.
//
// A failure is downgraded to a warning and an empty map rather than an error response:
// the mailing list itself was fetched successfully, and a database hiccup should cost
// the caller the profile decoration, not the roster.
func (h *Handler) lookupProfiles(ctx context.Context, logger *zap.Logger, emails []string) map[string]MemberProfile {
	users, err := h.profiles.ListByEmails(ctx, emails)
	if err != nil {
		logger.Warn("Failed to load local profiles for mailing list members", zap.Error(err))
		return nil
	}

	profiles := make(map[string]MemberProfile, len(users))
	for _, u := range users {
		profiles[strings.ToLower(u.Email)] = MemberProfile{
			Name:       u.Name,
			Nickname:   u.Nickname,
			Department: u.Department,
		}
	}

	return profiles
}

// newMemberResponse joins a Directory API member with its local profile, if any.
//
// This used to be a struct conversion, which only compiled while Member and
// MemberResponse had identical layouts. Profile has no counterpart on Member, so the
// mapping is now explicit; keep it in one place so the three handlers cannot drift.
func newMemberResponse(m Member, profiles map[string]MemberProfile) MemberResponse {
	res := MemberResponse{
		ID:     m.ID,
		Email:  m.Email,
		Role:   m.Role,
		Type:   m.Type,
		Status: m.Status,
	}

	if profile, ok := profiles[strings.ToLower(m.Email)]; ok {
		res.Profile = &profile
	}

	return res
}

// newGroupResponse joins a Google group with the club's display metadata.
//
// This used to be a struct conversion, which only compiled while Group and
// GroupResponse had identical layouts. DisplayName and Section have no counterpart on
// Group, so the mapping is now explicit.
func (h *Handler) newGroupResponse(g Group) GroupResponse {
	section := h.chart.SectionOf(g.Email)

	return GroupResponse{
		ID:                 g.ID,
		Email:              g.Email,
		Name:               g.Name,
		Description:        g.Description,
		DisplayName:        h.chart.Name(g.Email),
		Section:            GroupSection{Key: section.Key, Name: section.Name},
		DirectMembersCount: g.DirectMembersCount,
		AdminCreated:       g.AdminCreated,
		Aliases:            g.Aliases,
	}
}

func (h *Handler) ListGroupsHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "ListGroupsHandler")
	defer span.End()
	logger := h.logger.With(zap.String("handler", "ListGroupsHandler"))

	groups, err := h.store.ListGroups(traceCtx)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	// Organizational order, not the alphabetical order Google happens to return: a flat
	// A-Z list of 34 addresses carries no structure. Unclassified lists sort last, and
	// ties fall back to the key so the order is stable.
	sort.Slice(groups, func(i, j int) bool {
		oi, oj := h.chart.Order(groups[i].Email), h.chart.Order(groups[j].Email)
		if oi != oj {
			return oi < oj
		}
		return groups[i].Email < groups[j].Email
	})

	items := make([]GroupResponse, 0, len(groups))
	for _, g := range groups {
		items = append(items, h.newGroupResponse(g))
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, ListGroupsResponse{
		Items:      items,
		TotalItems: len(items),
	})
}
