package jwt

import (
	"context"
	"net/http"
	"nycu-sdc/club-manager/internal"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type Verifier interface {
	Parse(ctx context.Context, tokenString string) (User, error)
}

type Middleware struct {
	logger        *zap.Logger
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter

	verifier Verifier
}

func NewMiddleware(verifier Verifier, logger *zap.Logger, problemWriter *problem.HttpWriter) Middleware {
	return Middleware{
		logger:        logger,
		tracer:        otel.Tracer("middleware/jwt"),
		problemWriter: problemWriter,
		verifier:      verifier,
	}
}

func (m Middleware) HandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		traceCtx, span := m.tracer.Start(r.Context(), "JWTMiddleware")
		defer span.End()
		logger := logutil.WithContext(traceCtx, m.logger)

		token := r.Header.Get("Authorization")
		if token == "" {
			logger.Warn("Authorization header required")
			m.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrUnauthorized, logger)
			return
		}

		user, err := m.verifier.Parse(traceCtx, token)
		if err != nil {
			logger.Warn("Authorization header invalid", zap.Error(err))
			// Report the generic sentinel rather than the parse error, so token
			// internals are not echoed back to the caller.
			m.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrUnauthorized, logger)
			return
		}

		logger.Debug("Authorization header valid")
		r = r.WithContext(context.WithValue(traceCtx, internal.UserContextKey, user))
		next.ServeHTTP(w, r)
	}
}

// RequireRole rejects callers whose access token does not carry one of the given roles.
//
// It must be chained AFTER HandlerFunc, which is what puts the user into the context; on
// its own it fails closed, since a missing user is treated as no role at all.
//
// The role comes from the token claim, and the claim is derived from the caller's role
// in the login mailing list at sign-in (see internal/auth). Refreshing re-reads the user
// row but never re-reads the mailing list, so a promotion or demotion made in Google
// Groups reaches this check only when that person signs in again.
func (m Middleware) RequireRole(roles ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			traceCtx, span := m.tracer.Start(r.Context(), "RequireRoleMiddleware")
			defer span.End()
			logger := logutil.WithContext(traceCtx, m.logger)

			user, err := GetUserFromContext(traceCtx)
			if err != nil {
				logger.Error("RequireRole ran without a user in the context; it must be chained after the JWT middleware")
				m.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrForbidden, logger)
				return
			}

			for _, role := range roles {
				if user.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}

			logger.Warn("Rejected request for insufficient role",
				zap.String("user_id", user.ID.String()),
				zap.String("role", user.Role),
				zap.Strings("required", roles))
			m.problemWriter.WriteErrorWithRequest(traceCtx, r, w, handlerutil.ErrForbidden, logger)
		}
	}
}

func GetUserFromContext(ctx context.Context) (User, error) {
	user, ok := ctx.Value(internal.UserContextKey).(User)
	if !ok {
		return User{}, handlerutil.ErrInternalServer
	}
	return user, nil
}

func SetUserToContext(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, internal.UserContextKey, user)
}
