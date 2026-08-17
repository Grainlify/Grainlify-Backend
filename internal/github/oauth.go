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

type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// tokenEndpoint is a var (not a const) so tests can point it at an
// httptest.Server instead of the real GitHub API.
var tokenEndpoint = "https://github.com/login/oauth/access_token"

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

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`

	// GitHub answers a REFUSED exchange with HTTP 200 and an error payload
	// rather than a 4xx, so the status code alone cannot tell a rejected code
	// from an accepted one. Without these fields the three causes collapse
	// into one useless message:
	//
	//   bad_verification_code    the code was reused, or expired (10 min)
	//   incorrect_client_credentials  the client secret is wrong or rotated
	//   redirect_uri_mismatch    the callback URL no longer matches the app
	//
	// The middle one is a credential incident and the first is routine. They
	// are worth being able to tell apart at a glance.
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

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
		// Deliberately not newAPIError: a 2xx body from this endpoint carries
		// the access token, and an error formatter that ever prints the body
		// is one refactor away from printing a token. Only the parsed error
		// fields below are ever surfaced from here.
		return TokenResponse{}, fmt.Errorf("github token exchange failed: status %d", resp.StatusCode)
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return TokenResponse{}, err
	}
	if tr.ErrorCode != "" {
		return TokenResponse{}, fmt.Errorf("github token exchange refused: %s (%s)", tr.ErrorCode, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return TokenResponse{}, fmt.Errorf("github token exchange returned no token and no error")
	}
	return tr, nil
}
