package jwt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nycu-sdc/club-manager/internal/apperr"

	databaseutil "github.com/NYCU-SDC/summer/pkg/database"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// issuerName is the `iss` claim on tokens minted by this service.
const issuerName = "club-manager"

// stateLifetime bounds how long an OAuth login may stay in flight. It is the window in
// which a captured state parameter could be replayed, so keep it short.
const stateLifetime = 5 * time.Minute

// Access and state tokens are both HS256 signed with the same secret and both carry a
// UUID subject, so without an audience claim a state token -- which travels in URLs,
// browser history and Referer headers -- would be accepted as a session token. These
// separate the two; parsing enforces the expected audience.
const (
	audienceAccess = "club-manager:access"
	audienceState  = "club-manager:state"
)

type Querier interface {
	CreateRefreshToken(ctx context.Context, arg CreateRefreshTokenParams) (RefreshToken, error)
	GetRefreshTokenByID(ctx context.Context, id uuid.UUID) (RefreshToken, error)
	GetUserByRefreshToken(ctx context.Context, id uuid.UUID) (User, error)
	InactivateRefreshToken(ctx context.Context, id uuid.UUID) (RefreshToken, error)
	InactivateRefreshTokensByUserID(ctx context.Context, userID uuid.UUID) (int64, error)
	DeleteExpiredRefreshTokens(ctx context.Context) (int64, error)
}

// claims is the access token payload. Subject carries the user UUID.
type claims struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

// stateClaims is the OAuth state parameter, signed so the callback can trust the
// redirect target it carries.
type stateClaims struct {
	Redirect string `json:"redirect"`
	jwt.RegisteredClaims
}

// Service mints and verifies access tokens, and manages refresh tokens.
//
// Access tokens are stateless JWTs; refresh tokens are opaque row IDs in Postgres,
// which is what makes revocation possible at all.
type Service struct {
	logger *zap.Logger
	tracer trace.Tracer

	secret                 string
	expiration             time.Duration
	refreshTokenExpiration time.Duration

	queries Querier
}

func NewService(logger *zap.Logger, secret string, expiration, refreshTokenExpiration time.Duration, queries Querier) *Service {
	return &Service{
		logger:                 logger,
		tracer:                 otel.Tracer("jwt/service"),
		secret:                 secret,
		expiration:             expiration,
		refreshTokenExpiration: refreshTokenExpiration,
		queries:                queries,
	}
}

// Expiration reports the access token lifetime, for reporting expiresIn to clients.
func (s *Service) Expiration() time.Duration {
	return s.expiration
}

// New mints an access token for user.
func (s *Service) New(ctx context.Context, user User) (string, error) {
	traceCtx, span := s.tracer.Start(ctx, "New")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	now := time.Now()
	jwtID := uuid.New()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Email: user.Email,
		Role:  user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Audience:  jwt.ClaimStrings{audienceAccess},
			Subject:   user.ID.String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.expiration)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jwtID.String(),
		},
	})

	tokenString, err := token.SignedString([]byte(s.secret))
	if err != nil {
		logger.Error("Failed to sign access token", zap.Error(err), zap.String("user_id", user.ID.String()))
		span.RecordError(err)
		return "", err
	}

	return tokenString, nil
}

func (s *Service) Parse(ctx context.Context, tokenString string) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "Parse")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	tokenString = strings.TrimPrefix(tokenString, "Bearer ")

	token, err := jwt.ParseWithClaims(tokenString, &claims{}, func(token *jwt.Token) (any, error) {
		// Reject tokens that ask for a different algorithm, otherwise a caller could
		// present an unsigned or asymmetrically-signed token and bypass verification.
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return []byte(s.secret), nil
	}, jwt.WithAudience(audienceAccess))
	if err != nil {
		logParseError(logger, err)
		return User{}, err
	}

	parsed, ok := token.Claims.(*claims)
	if !ok {
		logger.Warn("JWT token has unexpected claims type")
		return User{}, jwt.ErrTokenInvalidClaims
	}

	var id uuid.UUID
	if parsed.Subject != "" {
		id, err = uuid.Parse(parsed.Subject)
		if err != nil {
			logger.Warn("JWT subject is not a valid UUID", zap.String("subject", parsed.Subject), zap.Error(err))
			return User{}, err
		}
	}

	return User{
		ID:    id,
		Email: parsed.Email,
		Role:  parsed.Role,
	}, nil
}

// NewState mints the OAuth state parameter, carrying the post-login redirect so the
// callback can trust it rather than reading it from an attacker-controlled query.
func (s *Service) NewState(ctx context.Context, redirect string) (string, error) {
	traceCtx, span := s.tracer.Start(ctx, "NewState")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	now := time.Now()
	jwtID := uuid.New()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, stateClaims{
		Redirect: redirect,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerName,
			Audience:  jwt.ClaimStrings{audienceState},
			Subject:   jwtID.String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(stateLifetime)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jwtID.String(),
		},
	})

	tokenString, err := token.SignedString([]byte(s.secret))
	if err != nil {
		logger.Error("Failed to sign state token", zap.Error(err))
		span.RecordError(err)
		return "", err
	}

	return tokenString, nil
}

// ParseState verifies the state parameter and returns the redirect it carries.
func (s *Service) ParseState(ctx context.Context, tokenString string) (string, error) {
	traceCtx, span := s.tracer.Start(ctx, "ParseState")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	parsed := &stateClaims{}
	_, err := jwt.ParseWithClaims(tokenString, parsed, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return []byte(s.secret), nil
	}, jwt.WithAudience(audienceState))
	if err != nil {
		logParseError(logger, err)
		return "", fmt.Errorf("%w: %v", apperr.ErrInvalidState, err)
	}

	return parsed.Redirect, nil
}

// GenerateRefreshToken issues a refresh token row for user.
func (s *Service) GenerateRefreshToken(ctx context.Context, user User) (RefreshToken, error) {
	traceCtx, span := s.tracer.Start(ctx, "GenerateRefreshToken")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	// Opportunistic cleanup; a failure here must not block issuing a token.
	if _, err := s.queries.DeleteExpiredRefreshTokens(traceCtx); err != nil {
		logger.Warn("Failed to delete expired refresh tokens", zap.Error(err))
	}

	refreshToken, err := s.queries.CreateRefreshToken(traceCtx, CreateRefreshTokenParams{
		UserID:         user.ID,
		ExpirationDate: pgtype.Timestamptz{Time: time.Now().Add(s.refreshTokenExpiration), Valid: true},
	})
	if err != nil {
		err = databaseutil.WrapDBError(err, logger, "generate refresh token")
		span.RecordError(err)
		return RefreshToken{}, err
	}

	return refreshToken, nil
}

// GetUserByRefreshToken resolves an active, unexpired refresh token to its user.
func (s *Service) GetUserByRefreshToken(ctx context.Context, id uuid.UUID) (User, error) {
	traceCtx, span := s.tracer.Start(ctx, "GetUserByRefreshToken")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	refreshToken, err := s.queries.GetRefreshTokenByID(traceCtx, id)
	if err != nil {
		err = databaseutil.WrapDBErrorWithKeyValue(err, "refresh_tokens", "id", id.String(), logger, "get refresh token")
		span.RecordError(err)
		return User{}, err
	}

	if !refreshToken.IsActive {
		err = fmt.Errorf("%w: refresh token is inactive", apperr.ErrInvalidRefreshToken)
		span.RecordError(err)
		return User{}, err
	}

	if refreshToken.ExpirationDate.Time.Before(time.Now()) {
		err = fmt.Errorf("%w: refresh token expired", apperr.ErrInvalidRefreshToken)
		span.RecordError(err)
		return User{}, err
	}

	user, err := s.queries.GetUserByRefreshToken(traceCtx, id)
	if err != nil {
		err = databaseutil.WrapDBError(err, logger, "get user by refresh token")
		span.RecordError(err)
		return User{}, err
	}

	return user, nil
}

func (s *Service) InactivateRefreshToken(ctx context.Context, id uuid.UUID) error {
	traceCtx, span := s.tracer.Start(ctx, "InactivateRefreshToken")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if _, err := s.queries.InactivateRefreshToken(traceCtx, id); err != nil {
		err = databaseutil.WrapDBError(err, logger, "inactivate refresh token")
		span.RecordError(err)
		return err
	}

	return nil
}

func (s *Service) InactivateRefreshTokensByUserID(ctx context.Context, userID uuid.UUID) error {
	traceCtx, span := s.tracer.Start(ctx, "InactivateRefreshTokensByUserID")
	defer span.End()
	logger := logutil.WithContext(traceCtx, s.logger)

	if _, err := s.queries.InactivateRefreshTokensByUserID(traceCtx, userID); err != nil {
		err = databaseutil.WrapDBError(err, logger, "inactivate refresh tokens by user")
		span.RecordError(err)
		return err
	}

	return nil
}

func logParseError(logger *zap.Logger, err error) {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		logger.Warn("Failed to parse JWT due to malformed structure", zap.String("error", err.Error()))
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		logger.Warn("Failed to parse JWT due to invalid signature", zap.String("error", err.Error()))
	case errors.Is(err, jwt.ErrTokenExpired):
		logger.Warn("Failed to parse JWT due to expired timestamp", zap.String("error", err.Error()))
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		logger.Warn("Failed to parse JWT due to not-valid-yet timestamp", zap.String("error", err.Error()))
	default:
		logger.Warn("Failed to parse or validate JWT", zap.Error(err))
	}
}
