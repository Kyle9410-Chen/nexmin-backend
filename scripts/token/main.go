// Command token mints a short-lived JWT for local testing of protected endpoints.
//
// The backend only verifies tokens; it never issues them. This helper stands in for
// whatever will eventually mint them, so protected routes can be exercised locally.
//
//	go run ./scripts/token                       # reads secret from config.yaml
//	go run ./scripts/token -role admin -ttl 24h
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func main() {
	var (
		secret = flag.String("secret", "", "signing secret (default: the secret from -config)")
		config = flag.String("config", "config.yaml", "config file to read the secret from")
		email  = flag.String("email", "dev@example.com", "email claim")
		role   = flag.String("role", "admin", "role claim")
		sub    = flag.String("sub", "", "subject; must be a UUID (default: a random one)")
		ttl    = flag.Duration("ttl", time.Hour, "token lifetime")
	)
	flag.Parse()

	if *secret == "" {
		s, err := secretFromConfig(*config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read secret from %s: %v\nPass -secret explicitly.\n", *config, err)
			os.Exit(1)
		}
		*secret = s
	}

	if *sub == "" {
		*sub = uuid.NewString()
	} else if _, err := uuid.Parse(*sub); err != nil {
		// Parse rejects a non-UUID subject, so fail here rather than emitting a token
		// that is guaranteed to 401.
		fmt.Fprintf(os.Stderr, "subject %q is not a valid UUID\n", *sub)
		os.Exit(1)
	}

	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		// Must match internal/jwt.audienceAccess; the server rejects tokens without it,
		// which is what stops an OAuth state token being replayed as a session.
		"aud":   "club-manager:access",
		"sub":   *sub,
		"email": *email,
		"role":  *role,
		"iat":   now.Unix(),
		"exp":   now.Add(*ttl).Unix(),
	}).SignedString([]byte(*secret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to sign token: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(token)
}

func secretFromConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var c struct {
		Secret string `yaml:"secret"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	if c.Secret == "" {
		return "", fmt.Errorf("no secret set in %s", path)
	}

	return c.Secret, nil
}
