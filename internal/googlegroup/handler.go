package googlegroup

import (
	"context"
	"net/http"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Store is the consumer-side view of the service this handler needs.
type Store interface {
	ListMembers(ctx context.Context, groupKey string) ([]Member, error)
	ListGroups(ctx context.Context) ([]Group, error)
}

type MemberResponse struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type ListMembersResponse struct {
	Items      []MemberResponse `json:"items"`
	TotalItems int              `json:"totalItems"`
}

type GroupResponse struct {
	ID                 string   `json:"id"`
	Email              string   `json:"email"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
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
	problemWriter *problem.HttpWriter
	tracer        trace.Tracer

	store Store
}

func NewHandler(logger *zap.Logger, problemWriter *problem.HttpWriter, store Store) *Handler {
	return &Handler{
		logger:        logger,
		problemWriter: problemWriter,
		tracer:        otel.Tracer("googlegroup/handler"),
		store:         store,
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

	items := make([]MemberResponse, 0, len(members))
	for _, m := range members {
		// A struct conversion only compiles while the two types have identical field
		// layouts, so if Member and MemberResponse ever diverge this stops building and
		// forces an explicit field-by-field mapping rather than silently reshaping the
		// JSON response.
		items = append(items, MemberResponse(m))
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, ListMembersResponse{
		Items:      items,
		TotalItems: len(items),
	})
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

	items := make([]GroupResponse, 0, len(groups))
	for _, g := range groups {
		// See the note in ListMembersHandler: the conversion only compiles while the
		// two types match, so divergence becomes a build error rather than a silently
		// reshaped response.
		items = append(items, GroupResponse(g))
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, ListGroupsResponse{
		Items:      items,
		TotalItems: len(items),
	})
}
