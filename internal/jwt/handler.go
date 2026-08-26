package jwt

import (
	"context"
	"net/http"
	"time"

	"nycu-sdc/nexmin/internal/apperr"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Issuer is the subset of Service this handler needs.
type Issuer interface {
	New(ctx context.Context, user User) (string, error)
	GenerateRefreshToken(ctx context.Context, user User) (RefreshToken, error)
	GetUserByRefreshToken(ctx context.Context, id uuid.UUID) (User, error)
	InactivateRefreshToken(ctx context.Context, id uuid.UUID) error
	InactivateRefreshTokensByUserID(ctx context.Context, userID uuid.UUID) error
	Expiration() time.Duration
}

type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" validate:"required,uuid"`
}

type TokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

type Handler struct {
	logger        *zap.Logger
	tracer        trace.Tracer
	validator     *validator.Validate
	problemWriter *problem.HttpWriter

	issuer Issuer
}

func NewHandler(logger *zap.Logger, validator *validator.Validate, problemWriter *problem.HttpWriter, issuer Issuer) *Handler {
	return &Handler{
		logger:        logger,
		tracer:        otel.Tracer("jwt/handler"),
		validator:     validator,
		problemWriter: problemWriter,
		issuer:        issuer,
	}
}

// RefreshHandler exchanges a refresh token for a new token pair.
//
// The refresh token arrives in the body rather than the URL so it stays out of access
// logs and Referer headers. It is rotated on every use: the presented token is
// inactivated, so a stolen token is usable at most once before the legitimate client's
// next refresh invalidates it.
func (h *Handler) RefreshHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "RefreshHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	var request RefreshRequest
	if err := handlerutil.ParseAndValidateRequestBody(traceCtx, h.validator, r, &request); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	id, err := uuid.Parse(request.RefreshToken)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, apperr.ErrInvalidRefreshToken, logger)
		return
	}

	user, err := h.issuer.GetUserByRefreshToken(traceCtx, id)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	accessToken, err := h.issuer.New(traceCtx, user)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	newRefreshToken, err := h.issuer.GenerateRefreshToken(traceCtx, user)
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	// Rotate only after the replacement exists, so a failure cannot leave the client
	// with no usable refresh token.
	if err := h.issuer.InactivateRefreshToken(traceCtx, id); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	handlerutil.WriteJSONResponse(w, http.StatusOK, TokenResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken.ID.String(),
		ExpiresIn:    int64(h.issuer.Expiration().Seconds()),
	})
}

// LogoutHandler revokes every refresh token belonging to the caller.
//
// Outstanding access tokens stay valid until they expire; that is inherent to stateless
// JWTs, and the short access lifetime is the mitigation.
func (h *Handler) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "LogoutHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	user, err := GetUserFromContext(r.Context())
	if err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrUnauthorized, logger)
		return
	}

	if err := h.issuer.InactivateRefreshTokensByUserID(traceCtx, user.ID); err != nil {
		h.problemWriter.WriteErrorWithRequest(traceCtx, r, w, err, logger)
		return
	}

	logger.Info("User signed out", zap.String("user_id", user.ID.String()))

	w.WriteHeader(http.StatusNoContent)
}
