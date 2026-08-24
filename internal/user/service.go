package user

import (
	"context"
	"errors"

	databaseutil "github.com/NYCU-SDC/summer/pkg/database"
	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type Service struct {
	logger *zap.Logger
	tracer trace.Tracer

	queries *Queries
}

func NewService(logger *zap.Logger, db DBTX) *Service {
	return &Service{
		logger:  logger,
		tracer:  otel.Tracer("user/service"),
		queries: New(db),
	}
}

func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "GetByID")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	user, err := s.queries.GetByID(traceCtx, id)
	if err != nil {
		err = databaseutil.WrapDBErrorWithKeyValue(err, "users", "id", id.String(), logger, "get user by id")
		span.RecordError(err)
		return User{}, err
	}

	return user, nil
}

// FindOrCreateByEmail returns the user with this email, creating one on first login.
//
// Email is the identity key because the login gate is mailing list membership, which is
// itself keyed on email. Lookup is case-insensitive to match Google's treatment.
func (s *Service) FindOrCreateByEmail(ctx context.Context, email, name string) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "FindOrCreateByEmail")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	user, err := s.queries.GetByEmail(traceCtx, email)
	if err == nil {
		return user, nil
	}

	wrapped := databaseutil.WrapDBErrorWithKeyValue(err, "users", "email", email, logger, "get user by email")
	if !errors.Is(wrapped, handlerutil.ErrNotFound) {
		span.RecordError(wrapped)
		return User{}, wrapped
	}

	user, err = s.queries.Create(traceCtx, CreateParams{Email: email, Name: name})
	if err != nil {
		err = databaseutil.WrapDBError(err, logger, "create user")
		span.RecordError(err)
		return User{}, err
	}

	logger.Info("Created user on first login", zap.String("user_id", user.ID.String()), zap.String("email", email))

	return user, nil
}
