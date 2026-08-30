package user

import (
	"context"
	"errors"
	"strings"

	databaseutil "github.com/NYCU-SDC/summer/pkg/database"
	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
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
//
// The role is recomputed from the mailing list on every sign-in and written back here,
// so demotions in Google Groups follow the user in as well as promotions. The mailing
// list is the only source of authority for it; see internal/auth.RoleResolver.
//
// name is different: Google only seeds it when the row is created. From then on it
// belongs to the user, who edits it through PATCH /api/users/me, so a later sign-in
// deliberately leaves it alone rather than overwriting their choice with the Google
// display name.
func (s *Service) FindOrCreateByEmail(ctx context.Context, email, name, role string) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "FindOrCreateByEmail")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	user, err := s.queries.GetByEmail(traceCtx, email)
	if err == nil {
		if user.Role == role {
			return user, nil
		}

		updated, updateErr := s.queries.UpdateRole(traceCtx, UpdateRoleParams{ID: user.ID, Role: role})
		if updateErr != nil {
			updateErr = databaseutil.WrapDBError(updateErr, logger, "update user role")
			span.RecordError(updateErr)
			return User{}, updateErr
		}

		logger.Info("Synced user role from the login group",
			zap.String("user_id", user.ID.String()),
			zap.String("email", email),
			zap.String("from", user.Role),
			zap.String("to", role))

		return updated, nil
	}

	wrapped := databaseutil.WrapDBErrorWithKeyValue(err, "users", "email", email, logger, "get user by email")
	if !errors.Is(wrapped, handlerutil.ErrNotFound) {
		span.RecordError(wrapped)
		return User{}, wrapped
	}

	user, err = s.queries.Create(traceCtx, CreateParams{Email: email, Name: name, Role: role})
	if err != nil {
		err = databaseutil.WrapDBError(err, logger, "create user")
		span.RecordError(err)
		return User{}, err
	}

	logger.Info("Created user on first login", zap.String("user_id", user.ID.String()), zap.String("email", email), zap.String("role", role))

	return user, nil
}

// ListByEmails returns the users whose email appears in emails, matched
// case-insensitively so callers can pass addresses straight from Google.
//
// Addresses with no local user are simply absent from the result: being on a mailing
// list does not imply having signed in here.
func (s *Service) ListByEmails(ctx context.Context, emails []string) ([]User, error) {
	if len(emails) == 0 {
		return nil, nil
	}

	traceCtx, span := s.tracer.Start(ctx, "ListByEmails")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	// The query compares lower(email), so lower the inputs to match.
	lowered := make([]string, 0, len(emails))
	for _, email := range emails {
		lowered = append(lowered, strings.ToLower(email))
	}

	users, err := s.queries.ListByEmails(traceCtx, lowered)
	if err != nil {
		err = databaseutil.WrapDBError(err, logger, "list users by emails")
		span.RecordError(err)
		return nil, err
	}

	return users, nil
}

// UpdateProfile writes the fields the user maintains themselves. A nil argument leaves
// the stored value untouched, which is what makes PATCH partial: the handler can tell
// "field omitted" from "field set to empty" and pass that distinction through.
func (s *Service) UpdateProfile(ctx context.Context, id uuid.UUID, name, nickname, department *string) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "UpdateProfile")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	user, err := s.queries.UpdateProfile(traceCtx, UpdateProfileParams{
		ID:         id,
		Name:       optionalText(name),
		Nickname:   optionalText(nickname),
		Department: optionalText(department),
	})
	if err != nil {
		err = databaseutil.WrapDBErrorWithKeyValue(err, "users", "id", id.String(), logger, "update user profile")
		span.RecordError(err)
		return User{}, err
	}

	return user, nil
}

// UpdateRole writes the role derived from the login mailing list back to the user row.
//
// The mailing list is the authority; users.role is a cache of it, refreshed at sign-in
// and whenever a read path notices the two have drifted apart.
func (s *Service) UpdateRole(ctx context.Context, id uuid.UUID, role string) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "UpdateRole")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	user, err := s.queries.UpdateRole(traceCtx, UpdateRoleParams{ID: id, Role: role})
	if err != nil {
		err = databaseutil.WrapDBErrorWithKeyValue(err, "users", "id", id.String(), logger, "update user role")
		span.RecordError(err)
		return User{}, err
	}

	return user, nil
}

// SeedProfile creates a row for someone who is on the club's mailing list but has not
// signed in yet, so the roster can show their name rather than a bare address. It
// reports whether a row was actually created.
//
// An existing row is never touched. Name and nickname belong to the user from the
// moment their row exists, exactly as in FindOrCreateByEmail, so "already there" is a
// success and not a conflict -- which is also what makes the startup sync idempotent.
func (s *Service) SeedProfile(ctx context.Context, email, name, nickname string) (bool, error) {
	traceCtx, span := s.tracer.Start(ctx, "SeedProfile")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	rows, err := s.queries.SeedProfile(traceCtx, SeedProfileParams{Email: email, Name: name, Nickname: nickname})
	if err != nil {
		err = databaseutil.WrapDBErrorWithKeyValue(err, "users", "email", email, logger, "seed user profile")
		span.RecordError(err)
		return false, err
	}

	return rows > 0, nil
}

// optionalText maps a nil pointer onto a NULL parameter, which the COALESCE in
// UpdateProfile reads as "leave this column alone".
func optionalText(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}
