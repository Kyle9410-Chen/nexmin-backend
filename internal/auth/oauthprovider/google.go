package oauthprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// UserInfo is the normalized identity returned by a provider.
type UserInfo struct {
	ID            string
	Email         string
	Name          string
	EmailVerified bool
}

type googleUserResponse struct {
	Sub           string `json:"sub"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

type GoogleConfig struct {
	config *oauth2.Config
}

func NewGoogleConfig(clientID, clientSecret, redirectURL string) *GoogleConfig {
	return &GoogleConfig{
		config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes: []string{
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
			},
			Endpoint: google.Endpoint,
		},
	}
}

func (g *GoogleConfig) Name() string { return "google" }

func (g *GoogleConfig) Config() *oauth2.Config { return g.config }

// Configured reports whether OAuth credentials were supplied.
func (g *GoogleConfig) Configured() bool {
	return g.config.ClientID != "" && g.config.ClientSecret != ""
}

func (g *GoogleConfig) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	return g.config.Exchange(ctx, code)
}

func (g *GoogleConfig) GetUserInfo(ctx context.Context, token *oauth2.Token) (UserInfo, error) {
	client := g.config.Client(ctx, token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return UserInfo{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return UserInfo{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, fmt.Errorf("userinfo request failed with status %d", resp.StatusCode)
	}

	var body googleUserResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return UserInfo{}, err
	}

	return UserInfo{
		ID:            body.Sub,
		Email:         body.Email,
		Name:          body.Name,
		EmailVerified: body.EmailVerified,
	}, nil
}
