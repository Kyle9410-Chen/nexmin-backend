package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"nycu-sdc/club-manager/internal/auth/oauthprovider"
	"nycu-sdc/club-manager/internal/googlegroup"
	"nycu-sdc/club-manager/internal/jwt"
	"nycu-sdc/club-manager/internal/user"

	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

// activeMemberStatus is the Directory API status for a usable membership. Suspended or
// unknown members must not be able to sign in.
const activeMemberStatus = "ACTIVE"

// MemberChecker resolves the members of the group that gates login.
type MemberChecker interface {
	ListMembers(ctx context.Context, groupKey string) ([]googlegroup.Member, error)
}

// UserStore looks up or creates the local user record for a verified identity.
type UserStore interface {
	FindOrCreateByEmail(ctx context.Context, email, name string) (user.User, error)
}

// TokenIssuer mints the session tokens handed back to the frontend.
type TokenIssuer interface {
	New(ctx context.Context, user jwt.User) (string, error)
	GenerateRefreshToken(ctx context.Context, user jwt.User) (jwt.RefreshToken, error)
	NewState(ctx context.Context, redirect string) (string, error)
	ParseState(ctx context.Context, tokenString string) (string, error)
}

type Handler struct {
	logger *zap.Logger
	tracer trace.Tracer

	provider    *oauthprovider.GoogleConfig
	members     MemberChecker
	users       UserStore
	tokens      TokenIssuer
	loginGroup  string
	frontendURL string
	expiresIn   int64
}

func NewHandler(
	logger *zap.Logger,
	provider *oauthprovider.GoogleConfig,
	members MemberChecker,
	users UserStore,
	tokens TokenIssuer,
	loginGroup string,
	frontendURL string,
	expiresIn int64,
) *Handler {
	return &Handler{
		logger:      logger,
		tracer:      otel.Tracer("auth/handler"),
		provider:    provider,
		members:     members,
		users:       users,
		tokens:      tokens,
		loginGroup:  loginGroup,
		frontendURL: frontendURL,
		expiresIn:   expiresIn,
	}
}

// LoginHandler starts the OAuth flow by redirecting to Google.
func (h *Handler) LoginHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "LoginHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	if !h.configured() {
		logger.Warn("Login attempted while OAuth or login group is not configured")
		h.redirectError(w, r, "login_not_configured")
		return
	}

	state, err := h.tokens.NewState(traceCtx, r.URL.Query().Get("redirect"))
	if err != nil {
		logger.Error("Failed to create state token", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "server_error")
		return
	}

	authURL := h.provider.Config().AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// CallbackHandler completes the OAuth flow, enforces the mailing list gate, and hands
// the session tokens to the frontend.
func (h *Handler) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "CallbackHandler")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	if !h.configured() {
		h.redirectError(w, r, "login_not_configured")
		return
	}

	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		logger.Warn("Google returned an OAuth error", zap.String("error", oauthErr))
		h.redirectError(w, r, "oauth_denied")
		return
	}

	redirect, err := h.tokens.ParseState(traceCtx, r.URL.Query().Get("state"))
	if err != nil {
		logger.Warn("Invalid OAuth state", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "invalid_state")
		return
	}

	token, err := h.provider.Exchange(traceCtx, r.URL.Query().Get("code"))
	if err != nil {
		logger.Error("Failed to exchange authorization code", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "exchange_failed")
		return
	}

	info, err := h.provider.GetUserInfo(traceCtx, token)
	if err != nil {
		logger.Error("Failed to fetch Google user info", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "userinfo_failed")
		return
	}

	// The login gate matches on email, so an unverified address would let anyone who
	// can set that field impersonate a real member.
	if !info.EmailVerified {
		logger.Warn("Rejected sign-in for unverified email", zap.String("email", info.Email))
		h.redirectError(w, r, "email_not_verified")
		return
	}

	allowed, err := h.isMember(traceCtx, info.Email)
	if err != nil {
		logger.Error("Failed to check login group membership", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "membership_check_failed")
		return
	}
	if !allowed {
		logger.Warn("Rejected sign-in for non-member", zap.String("email", info.Email), zap.String("login_group", h.loginGroup))
		h.redirectError(w, r, "not_a_member")
		return
	}

	localUser, err := h.users.FindOrCreateByEmail(traceCtx, info.Email, info.Name)
	if err != nil {
		logger.Error("Failed to find or create user", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "server_error")
		return
	}

	// user.User and jwt.User are the same sqlc-generated shape in two packages; the
	// conversion stops compiling if either drifts.
	tokenUser := jwt.User(localUser)

	accessToken, err := h.tokens.New(traceCtx, tokenUser)
	if err != nil {
		logger.Error("Failed to mint access token", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "server_error")
		return
	}

	refreshToken, err := h.tokens.GenerateRefreshToken(traceCtx, tokenUser)
	if err != nil {
		logger.Error("Failed to create refresh token", zap.Error(err))
		span.RecordError(err)
		h.redirectError(w, r, "server_error")
		return
	}

	logger.Info("User signed in", zap.String("user_id", localUser.ID.String()), zap.String("email", localUser.Email))

	// Tokens go in the fragment, not the query string: browsers never send fragments to
	// servers, so they stay out of access logs, Referer headers, and proxy records.
	params := url.Values{}
	params.Set("accessToken", accessToken)
	params.Set("refreshToken", refreshToken.ID.String())
	params.Set("expiresIn", fmt.Sprintf("%d", h.expiresIn))
	if redirect != "" {
		params.Set("redirect", redirect)
	}

	http.Redirect(w, r, h.frontendURL+"#"+params.Encode(), http.StatusTemporaryRedirect)
}

// isMember reports whether email is an active direct member of the login group.
func (h *Handler) isMember(ctx context.Context, email string) (bool, error) {
	members, err := h.members.ListMembers(ctx, h.loginGroup)
	if err != nil {
		return false, err
	}

	for _, m := range members {
		if strings.EqualFold(m.Email, email) && m.Status == activeMemberStatus {
			return true, nil
		}
	}

	return false, nil
}

func (h *Handler) configured() bool {
	return h.provider.Configured() && h.loginGroup != "" && h.frontendURL != ""
}

// redirectError sends the user back to the frontend with a machine-readable reason.
// It deliberately never reveals whether an address exists in the group.
func (h *Handler) redirectError(w http.ResponseWriter, r *http.Request, reason string) {
	if h.frontendURL == "" {
		http.Error(w, reason, http.StatusInternalServerError)
		return
	}

	params := url.Values{}
	params.Set("error", reason)
	http.Redirect(w, r, h.frontendURL+"#"+params.Encode(), http.StatusTemporaryRedirect)
}
