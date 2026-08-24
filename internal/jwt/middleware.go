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
