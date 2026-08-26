package membership

import (
	"context"
	"net/http"

	"nycu-sdc/club-manager/internal/jwt"
	"nycu-sdc/club-manager/internal/orgchart"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Store is the consumer-side view of the service this handler needs.
type Store interface {
	ForEmail(ctx context.Context, email string) (Result, error)
}

// Chart is the display metadata this handler reads. Satisfied by *orgchart.Chart.
type Chart interface {
	OwnerOf(key string) (orgchart.Role, bool)
}

type RoleResponse struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type ItemResponse struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	MemberCount int64  `json:"memberCount"`

	// OwnerRole is the officer responsible for this list, null for lists no role owns.
	OwnerRole *RoleResponse `json:"ownerRole"`
}

type SectionResponse struct {
	Key   string         `json:"key"`
	Name  string         `json:"name"`
	Items []ItemResponse `json:"items"`
}

type Response struct {
	Sections   []SectionResponse `json:"sections"`
	TotalItems int               `json:"totalItems"`

	// Leadership says the caller holds an officer position, but not which one: every
	// office holder is MANAGER in every department group, so Google cannot tell the six
	// positions apart. See docs/organization.md.
	Leadership bool `json:"leadership"`
}

type Handler struct {
	logger        *zap.Logger
	problemWriter *problem.HttpWriter
	tracer        trace.Tracer

	store Store
	chart Chart
}

func NewHandler(logger *zap.Logger, problemWriter *problem.HttpWriter, store Store, chart Chart) *Handler {
	return &Handler{
		logger:        logger,
		problemWriter: problemWriter,
		tracer:        otel.Tracer("membership/handler"),
		store:         store,
		chart:         chart,
	}
}

// MyGroupsHandler returns the mailing lists the caller is listed on, grouped by the
// club's structure.
//
// Direct membership only: nested lists are not expanded. See membership.Service.expand.
//
// Everything it reports comes from Google, so unlike GET /api/users/me there is nothing
// to degrade to: an unconfigured or unavailable Directory API surfaces as 503.
func (h *Handler) MyGroupsHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "MyGroupsHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	caller, err := jwt.GetUserFromContext(traceCtx)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrUnauthorized, logger)
		return
	}

	result, err := h.store.ForEmail(traceCtx, caller.Email)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	sections := make([]SectionResponse, 0, len(result.Sections))
	total := 0
	for _, s := range result.Sections {
		items := make([]ItemResponse, 0, len(s.Items))
		for _, m := range s.Items {
			items = append(items, ItemResponse{
				Key:         m.Key,
				Name:        m.Name,
				MemberCount: m.MemberCount,
				OwnerRole:   h.ownerRole(m.Key),
			})
		}
		total += len(items)
		sections = append(sections, SectionResponse{Key: s.Key, Name: s.Name, Items: items})
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, Response{
		Sections:   sections,
		TotalItems: total,
		Leadership: result.Leadership,
	})
}

func (h *Handler) ownerRole(key string) *RoleResponse {
	role, ok := h.chart.OwnerOf(key)
	if !ok {
		return nil
	}
	return &RoleResponse{Key: role.Key, Name: role.Name}
}
