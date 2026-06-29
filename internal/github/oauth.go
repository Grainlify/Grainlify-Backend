package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const tokenEndpoint = "https://github.com/login/oauth/access_token"

// OAuthConfig holds the configuration for GitHub OAuth authentication.
// ClientID and ClientSecret are obtained from GitHub OAuth app settings.
// RedirectURL must match the callback URL configured in the GitHub OAuth app.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

func AuthorizeURL(clientID string, redirectURL string, state string, scopes []string) (string, error) {
	if clientID == "" || redirectURL == "" {
		return "", fmt.Errorf("github oauth not configured")
	}
	u, _ := url.Parse("https://github.com/login/oauth/authorize")
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURL)
	q.Set("state", state)
	if len(scopes) > 0 {
		// GitHub expects space-separated scopes
		q.Set("scope", joinScopes(scopes))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func joinScopes(scopes []string) string {
	out := ""
	for i, s := range scopes {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// TokenResponse represents the response from GitHub's OAuth token endpoint.
// AccessToken is the OAuth bearer token used to authenticate API requests.
// TokenType is typically "bearer".
// Scope indicates the granted OAuth scopes.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// ExchangeCode exchanges an OAuth authorization code for an access token.
// It makes a POST request to GitHub's token endpoint with the code and client credentials.
// The context can be used to set a deadline for the HTTP request (default timeout is 10s).
// Returns an error if the configuration is incomplete, code is empty, or the request fails.
// The access token in the response must be non-empty.
func ExchangeCode(ctx context.Context, code string, cfg OAuthConfig) (TokenResponse, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return TokenResponse{}, fmt.Errorf("github oauth not configured")
	}
	if code == "" {
		return TokenResponse{}, fmt.Errorf("code is required")
	}

	body := map[string]string{
		"client_id":     cfg.ClientID,
		"client_secret": cfg.ClientSecret,
		"code":          code,
		"redirect_uri":  cfg.RedirectURL,
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, bytes.NewReader(b))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return TokenResponse{}, err
	}
	if tr.AccessToken == "" {
		return TokenResponse{}, fmt.Errorf("token exchange returned empty token")
	}
	return tr, nil
}
