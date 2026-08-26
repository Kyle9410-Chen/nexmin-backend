package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"nycu-sdc/nexmin/internal"
	"nycu-sdc/nexmin/internal/auth"
	"nycu-sdc/nexmin/internal/auth/oauthprovider"
	"nycu-sdc/nexmin/internal/config"
	"nycu-sdc/nexmin/internal/cors"
	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/jwt"
	"nycu-sdc/nexmin/internal/membership"
	"nycu-sdc/nexmin/internal/orgchart"
	"nycu-sdc/nexmin/internal/trace"
	"nycu-sdc/nexmin/internal/user"
	"os"
	"os/signal"
	"syscall"
	"time"

	databaseutil "github.com/NYCU-SDC/summer/pkg/database"
	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

var AppName = "no-app-name"

var Version = "no-version"

var BuildTime = "no-build-time"

var CommitHash = "no-commit-hash"

var Environment = "no-env"

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 24 * time.Hour
)

func main() {
	AppName = os.Getenv("APP_NAME")
	if AppName == "" {
		AppName = "nexmin"
	}

	if BuildTime == "no-build-time" {
		now := time.Now()
		BuildTime = "not provided (now: " + now.Format(time.RFC3339) + ")"
	}

	Environment = os.Getenv("ENV")
	if Environment == "" {
		Environment = "no-env"
	}

	appMetadata := []zap.Field{
		zap.String("app_name", AppName),
		zap.String("version", Version),
		zap.String("build_time", BuildTime),
		zap.String("commit_hash", CommitHash),
		zap.String("environment", Environment),
	}

	cfg, cfgLog := config.Load()
	err := cfg.Validate()
	if err != nil {
		if errors.Is(err, config.ErrDatabaseURLRequired) {
			title := "Database URL is required"
			message := "Please set the DATABASE_URL environment variable or provide a config file with the database_url key."
			message = EarlyApplicationFailed(title, message)
			log.Fatal(message)
		} else {
			log.Fatalf("Failed to validate config: %v, exiting...", err)
		}
	}

	logger, err := initLogger(&cfg, appMetadata)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v, exiting...", err)
	}

	cfgLog.FlushToZap(logger)

	if cfg.Secret == config.DefaultSecret && !cfg.Debug {
		logger.Warn("Default secret detected in production environment, replace it with a secure random string")
		cfg.Secret = uuid.New().String()
	}

	logger.Info("Application initialization", zap.Bool("debug", cfg.Debug), zap.String("host", cfg.Host), zap.String("port", cfg.Port))

	logger.Info("Starting database migration...")

	err = databaseutil.MigrationUp(cfg.MigrationSource, cfg.DatabaseURL, logger)
	if err != nil {
		// golang-migrate's file source reports an empty migration directory as
		// fs.ErrNotExist (from source.Driver.First). No migrations are authored yet,
		// so treat that as "nothing to apply" instead of a startup failure. Once the
		// first migration lands, First succeeds and this branch stops firing.
		if errors.Is(err, fs.ErrNotExist) {
			logger.Warn("No migrations found, skipping migration", zap.String("source", cfg.MigrationSource))
		} else {
			logger.Fatal("Failed to run database migration", zap.Error(err))
		}
	}

	dbPool, err := initDatabasePool(cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("Failed to initialize database pool", zap.Error(err))
	}
	defer dbPool.Close()

	problemWriter := internal.NewProblemWriter()

	// Service
	googleGroupService, err := googlegroup.NewService(logger, cfg.GoogleGroup)
	if err != nil {
		logger.Fatal("Failed to initialize Google group service", zap.Error(err))
	}
	userService := user.NewService(logger, dbPool)
	jwtService := jwt.NewService(logger, cfg.Secret, accessTokenTTL, refreshTokenTTL, jwt.New(dbPool))

	googleProvider := oauthprovider.NewGoogleConfig(
		cfg.GoogleOauthClientID,
		cfg.GoogleOauthClientSecret,
		cfg.BaseURL+"/api/auth/google/callback",
	)
	if !googleProvider.Configured() {
		logger.Warn("Google OAuth is not configured, login endpoints will be unavailable")
	}
	if cfg.GoogleGroup.LoginGroup == "" {
		logger.Warn("No login group configured, login will be refused for everyone")
	}

	// roleResolver is the single implementation of "what may this member do here",
	// shared by the sign-in gate and the user read paths so they cannot disagree.
	roleResolver := auth.NewRoleResolver(logger, googleGroupService, cfg.GoogleGroup.LoginGroup)

	// Every error Load reports is a typo in a committed file, so fail here rather than
	// serving blank labels on the first request that needs them.
	chart, err := orgchart.Load()
	if err != nil {
		logger.Fatal("Failed to load the organization chart", zap.Error(err))
	}
	membershipService := membership.NewService(logger, googleGroupService, chart, cfg.GoogleGroup.LoginGroup)

	// Handler
	validate := validator.New()
	googleGroupHandler := googlegroup.NewHandler(logger, validate, problemWriter, googleGroupService, userService, chart)
	userHandler := user.NewHandler(logger, validate, problemWriter, userService, roleResolver, membershipService, membershipService)
	membershipHandler := membership.NewHandler(logger, problemWriter, membershipService, chart)
	jwtHandler := jwt.NewHandler(logger, validate, problemWriter, jwtService)
	authHandler := auth.NewHandler(
		logger,
		googleProvider,
		roleResolver,
		userService,
		jwtService,
		cfg.FrontendURL,
		int64(accessTokenTTL.Seconds()),
	)

	// Middleware
	traceMiddleware := trace.NewMiddleware(logger, cfg.Debug)
	corsMiddleware := cors.NewMiddleware(logger, cfg.AllowOrigins)
	jwtMiddleware := jwt.NewMiddleware(jwtService, logger, problemWriter)

	// Basic Middleware (Tracing and Recover)
	basicMiddleware := middleware.NewSet(traceMiddleware.RecoverMiddleware)
	basicMiddleware = basicMiddleware.Append(traceMiddleware.TraceMiddleWare)

	// Auth Middleware (JWT verification)
	authMiddleware := middleware.NewSet(traceMiddleware.RecoverMiddleware)
	authMiddleware = authMiddleware.Append(traceMiddleware.TraceMiddleWare)
	authMiddleware = authMiddleware.Append(jwtMiddleware.HandlerFunc)

	// Admin Middleware (JWT verification + role check). The role is derived from the
	// caller's role in the login mailing list at sign-in; see internal/auth.
	adminMiddleware := authMiddleware.Append(jwtMiddleware.RequireRole(user.RoleAdmin))

	// HTTP Server
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /api/healthz", basicMiddleware.HandlerFunc(healthz))

	// Auth
	mux.HandleFunc("GET /api/auth/google/login", basicMiddleware.HandlerFunc(authHandler.LoginHandler))
	mux.HandleFunc("GET /api/auth/google/callback", basicMiddleware.HandlerFunc(authHandler.CallbackHandler))
	mux.HandleFunc("POST /api/auth/refresh", basicMiddleware.HandlerFunc(jwtHandler.RefreshHandler))
	mux.HandleFunc("POST /api/auth/logout", authMiddleware.HandlerFunc(jwtHandler.LogoutHandler))

	// Users. /api/users/me is a literal segment, which Go's ServeMux prefers over the
	// {user_id} wildcard, so the two routes do not conflict.
	mux.HandleFunc("GET /api/users/me", authMiddleware.HandlerFunc(userHandler.MeHandler))
	mux.HandleFunc("GET /api/users/me/groups", authMiddleware.HandlerFunc(membershipHandler.MyGroupsHandler))
	mux.HandleFunc("PATCH /api/users/me", authMiddleware.HandlerFunc(userHandler.UpdateMeHandler))
	mux.HandleFunc("GET /api/users", adminMiddleware.HandlerFunc(userHandler.ListHandler))
	mux.HandleFunc("POST /api/users", adminMiddleware.HandlerFunc(userHandler.AddHandler))
	mux.HandleFunc("DELETE /api/users/{email}", adminMiddleware.HandlerFunc(userHandler.RemoveHandler))
	mux.HandleFunc("GET /api/users/{user_id}", adminMiddleware.HandlerFunc(userHandler.GetHandler))

	// Google groups
	mux.HandleFunc("GET /api/groups", authMiddleware.HandlerFunc(googleGroupHandler.ListGroupsHandler))
	mux.HandleFunc("GET /api/groups/{group_key}/members", authMiddleware.HandlerFunc(googleGroupHandler.ListMembersHandler))
	mux.HandleFunc("POST /api/groups/{group_key}/members", adminMiddleware.HandlerFunc(googleGroupHandler.AddMemberHandler))
	mux.HandleFunc("PATCH /api/groups/{group_key}/members/{member_key}", adminMiddleware.HandlerFunc(googleGroupHandler.UpdateMemberHandler))
	mux.HandleFunc("DELETE /api/groups/{group_key}/members/{member_key}", adminMiddleware.HandlerFunc(googleGroupHandler.RemoveMemberHandler))

	// handle interrupt signal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// CORS Middleware
	entrypoint := corsMiddleware.HandlerFunc(mux.ServeHTTP)

	srv := &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: entrypoint,
	}

	go func() {
		logger.Info("Starting listening request", zap.String("host", cfg.Host), zap.String("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("Fail to start server with error", zap.Error(err))
		}
	}()

	// wait for context close
	<-ctx.Done()
	logger.Info("Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown", zap.Error(err))
	}

	logger.Info("Successfully shutdown")
}

func healthz(w http.ResponseWriter, r *http.Request) {
	handlerutil.WriteJSONResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func initLogger(cfg *config.Config, appMetadata []zap.Field) (*zap.Logger, error) {
	var err error
	var logger *zap.Logger
	if cfg.Debug {
		logger, err = logutil.ZapDevelopmentConfig().Build()
		if err != nil {
			return nil, err
		}
		logger.Info("Running in debug mode", appMetadata...)
	} else {
		logger, err = logutil.ZapProductionConfig().Build()
		if err != nil {
			return nil, err
		}

		logger = logger.With(appMetadata...)
	}
	defer func() {
		err := logger.Sync()
		if err != nil {
			zap.S().Errorw("Failed to sync logger", zap.Error(err))
		}
	}()

	return logger, nil
}

func initDatabasePool(databaseURL string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}

	dbPool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, err
	}
	return dbPool, nil
}

func EarlyApplicationFailed(title, action string) string {
	result := `
-----------------------------------------
Application Failed to Start
-----------------------------------------

# What's wrong?
%s

# How to fix it?
%s

`

	result = fmt.Sprintf(result, title, action)
	return result
}
