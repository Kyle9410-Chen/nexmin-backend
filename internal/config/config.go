package config

import (
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"strings"

	"nycu-sdc/club-manager/internal/googlegroup"

	configutil "github.com/NYCU-SDC/summer/pkg/config"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const DefaultSecret = "default-secret"

var ErrDatabaseURLRequired = errors.New("database_url is required")

type Config struct {
	Debug           bool     `yaml:"debug"            envconfig:"DEBUG"`
	Host            string   `yaml:"host"             envconfig:"HOST"`
	Port            string   `yaml:"port"             envconfig:"PORT"`
	BaseURL         string   `yaml:"base_url"         envconfig:"BASE_URL"`
	Secret          string   `yaml:"secret"           envconfig:"SECRET"`
	DatabaseURL     string   `yaml:"database_url"     envconfig:"DATABASE_URL"`
	MigrationSource string   `yaml:"migration_source" envconfig:"MIGRATION_SOURCE"`
	AllowOrigins    []string `yaml:"allow_origins"    envconfig:"ALLOW_ORIGINS"`
	FrontendURL     string   `yaml:"frontend_url"     envconfig:"FRONTEND_URL"`

	GoogleOauthClientID     string `yaml:"google_oauth_client_id"     envconfig:"GOOGLE_OAUTH_CLIENT_ID"`
	GoogleOauthClientSecret string `yaml:"google_oauth_client_secret" envconfig:"GOOGLE_OAUTH_CLIENT_SECRET"`

	GoogleGroup googlegroup.Config `yaml:"google_group"`
}

type LogBuffer struct {
	buffer []logEntry
}

type logEntry struct {
	msg  string
	err  error
	meta map[string]string
}

func NewConfigLogger() *LogBuffer {
	return &LogBuffer{}
}

func (cl *LogBuffer) Warn(msg string, err error, meta map[string]string) {
	cl.buffer = append(cl.buffer, logEntry{msg: msg, err: err, meta: meta})
}

func (cl *LogBuffer) FlushToZap(logger *zap.Logger) {
	for _, e := range cl.buffer {
		var fields []zap.Field
		if e.err != nil {
			fields = append(fields, zap.Error(e.err))
		}
		for k, v := range e.meta {
			fields = append(fields, zap.String(k, v))
		}
		logger.Warn(e.msg, fields...)
	}
	cl.buffer = nil
}

func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return ErrDatabaseURLRequired
	}

	return nil
}

func Load() (Config, *LogBuffer) {
	logger := NewConfigLogger()

	config := &Config{
		Debug:           false,
		Host:            "localhost",
		Port:            "8080",
		Secret:          DefaultSecret,
		DatabaseURL:     "",
		MigrationSource: "file://internal/database/migrations",
		BaseURL:         "http://localhost:8080",
		FrontendURL:     "http://localhost:3000",
		GoogleGroup: googlegroup.Config{
			CacheTTL: "5m",
			// The club runs one Workspace domain, so default to it and let the API
			// speak bare group names out of the box. Override it anywhere the groups
			// live somewhere else; set it empty to keep full addresses.
			Domain: "sdc.nycu.club",
		},
	}

	var err error

	config, err = FromFile("config.yaml", config, logger)
	if err != nil {
		logger.Warn("Failed to load config from file", err, map[string]string{"path": "config.yaml"})
	}

	config, err = FromEnv(config, logger)
	if err != nil {
		logger.Warn("Failed to load config from env", err, map[string]string{"path": ".env"})
	}

	config, err = FromFlags(config)
	if err != nil {
		logger.Warn("Failed to load config from flags", err, map[string]string{"path": "flags"})
	}

	return *config, logger
}

// mergeConfig merges override into base, including nested sub-configs.
//
// configutil.Merge compares each top-level field against its zero value, so a nested
// struct is treated as a single field: if any one of its members is set, the entire
// struct replaces the base and every value from earlier layers is lost. Setting only
// GOOGLE_IMPERSONATE_SUBJECT would otherwise discard a service account key that came
// from config.yaml. Merging the nested config separately keeps per-field layering.
func mergeConfig(base, override *Config) (*Config, error) {
	googleGroup, err := configutil.Merge[googlegroup.Config](&base.GoogleGroup, &override.GoogleGroup)
	if err != nil {
		return nil, err
	}
	// Copy by value: Merge mutates base in place, so the outer merge below would
	// otherwise clobber what this pointer refers to.
	mergedGoogleGroup := *googleGroup

	merged, err := configutil.Merge[Config](base, override)
	if err != nil {
		return nil, err
	}
	merged.GoogleGroup = mergedGoogleGroup

	return merged, nil
}

func FromFile(filePath string, config *Config, logger *LogBuffer) (*Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return config, err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			logger.Warn("Failed to close config file", err, map[string]string{"path": filePath})
		}
	}(file)

	fileConfig := Config{}
	if err := yaml.NewDecoder(file).Decode(&fileConfig); err != nil {
		return config, err
	}

	return mergeConfig(config, &fileConfig)
}

func FromEnv(config *Config, logger *LogBuffer) (*Config, error) {
	if err := godotenv.Overload(); err != nil {
		if os.IsNotExist(err) {
			logger.Warn("No .env file found", err, map[string]string{"path": ".env"})
		} else {
			return nil, err
		}
	}

	// Allow origins
	allowOrigins := os.Getenv("ALLOW_ORIGINS")
	if allowOrigins != "" {
		config.AllowOrigins = strings.Split(allowOrigins, ",")
	}

	// The service account key is a JSON blob, so it travels base64-encoded in a single
	// env var to fit the deployment pipeline's scalar-string secret injection.
	serviceAccountKey := os.Getenv("GOOGLE_SERVICE_ACCOUNT_KEY")
	if serviceAccountKey != "" {
		if _, decodeErr := base64.StdEncoding.DecodeString(serviceAccountKey); decodeErr != nil {
			logger.Warn("GOOGLE_SERVICE_ACCOUNT_KEY is not valid base64, ignoring it", decodeErr, nil)
			serviceAccountKey = ""
		}
	}

	envConfig := &Config{
		Debug:           os.Getenv("DEBUG") == "true",
		Host:            os.Getenv("HOST"),
		Port:            os.Getenv("PORT"),
		BaseURL:         os.Getenv("BASE_URL"),
		Secret:          os.Getenv("SECRET"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		MigrationSource: os.Getenv("MIGRATION_SOURCE"),
		FrontendURL:     os.Getenv("FRONTEND_URL"),

		GoogleOauthClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleOauthClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),

		GoogleGroup: googlegroup.Config{
			ServiceAccountKey:  serviceAccountKey,
			ImpersonateSubject: os.Getenv("GOOGLE_IMPERSONATE_SUBJECT"),
			CacheTTL:           os.Getenv("GOOGLE_GROUP_CACHE_TTL"),
			Domain:             os.Getenv("GOOGLE_GROUP_DOMAIN"),
			LoginGroup:         os.Getenv("GOOGLE_LOGIN_GROUP"),
		},
	}

	return mergeConfig(config, envConfig)
}

func FromFlags(config *Config) (*Config, error) {
	flagConfig := &Config{}

	flag.BoolVar(&flagConfig.Debug, "debug", false, "debug mode")
	flag.StringVar(&flagConfig.Host, "host", "", "host")
	flag.StringVar(&flagConfig.Port, "port", "", "port")
	flag.StringVar(&flagConfig.BaseURL, "base_url", "", "base url")
	flag.StringVar(&flagConfig.Secret, "secret", "", "secret")
	flag.StringVar(&flagConfig.DatabaseURL, "database_url", "", "database url")
	flag.StringVar(&flagConfig.MigrationSource, "migration_source", "", "migration source")
	flag.StringVar(&flagConfig.GoogleGroup.ServiceAccountKey, "google_service_account_key", "", "base64-encoded google service account JSON key")
	flag.StringVar(&flagConfig.GoogleGroup.ImpersonateSubject, "google_impersonate_subject", "", "workspace admin to impersonate via domain-wide delegation")
	flag.StringVar(&flagConfig.GoogleGroup.CacheTTL, "google_group_cache_ttl", "", "mailing list member cache TTL, e.g. 5m")
	flag.StringVar(&flagConfig.GoogleGroup.Domain, "google_group_domain", "", "workspace domain the club's groups live in, e.g. sdc.nycu.club")
	flag.StringVar(&flagConfig.GoogleGroup.LoginGroup, "google_login_group", "", "mailing list whose members may sign in")
	flag.StringVar(&flagConfig.FrontendURL, "frontend_url", "", "frontend URL to redirect to after login")
	flag.StringVar(&flagConfig.GoogleOauthClientID, "google_oauth_client_id", "", "google OAuth client ID")
	flag.StringVar(&flagConfig.GoogleOauthClientSecret, "google_oauth_client_secret", "", "google OAuth client secret")

	flag.Parse()

	return mergeConfig(config, flagConfig)
}
