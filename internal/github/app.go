package github

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

// ErrInstallationNotFound is the sentinel error returned by GetInstallationToken when
// the GitHub App installation no longer exists (HTTP 404). Callers should detect this
// condition with errors.Is(err, github.ErrInstallationNotFound) rather than matching
// substrings in the error message.
var ErrInstallationNotFound = errors.New("github app installation not found")

// InstallationNotFoundError wraps ErrInstallationNotFound and carries the raw HTTP
// status code so callers can distinguish a genuine 404 from other error types.
type InstallationNotFoundError struct {
	InstallationID string
	StatusCode     int
}

func (e *InstallationNotFoundError) Error() string {
	return fmt.Sprintf("github app installation %s not found (HTTP %d)", e.InstallationID, e.StatusCode)
}

// Unwrap makes errors.Is(err, ErrInstallationNotFound) work for wrapped errors.
func (e *InstallationNotFoundError) Unwrap() error {
	return ErrInstallationNotFound
}

// Clock provides the current time, allowing for testability
type Clock interface {
	Now() time.Time
}

// realClock uses time.Now()
type realClock struct{}

func (c realClock) Now() time.Time {
	return time.Now()
}

// tokenRefreshAheadWindow is how far before a cached installation token's
// expiry we proactively fetch a fresh one.  GitHub tokens last ~1 hour;
// refreshing 5 minutes early gives callers a comfortable buffer against
// clock-skew and in-flight request latency without hammering the API.
//
// Example: a token that expires at T=60m is treated as stale at T=55m
// and will be transparently replaced on the next call to GetInstallationToken.
const tokenRefreshAheadWindow = 5 * time.Minute

// cachedToken holds a GitHub installation access token together with its
// expiry so the cache can decide when to refresh.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// isValid reports whether the token is still usable: it must not be empty
// and its expiry must be more than tokenRefreshAheadWindow away from now.
// Using the clock from the parent client keeps behaviour deterministic in tests.
func (ct cachedToken) isValid(now time.Time) bool {
	return ct.token != "" && now.Before(ct.expiresAt.Add(-tokenRefreshAheadWindow))
}

// GitHubAppClient handles GitHub App API calls
type GitHubAppClient struct {
	AppID      string
	PrivateKey *rsa.PrivateKey
	HTTP       *http.Client
	UserAgent  string
	BaseURL    string
	Clock      Clock

	// tokenCacheMu guards tokenCache.
	tokenCacheMu sync.Mutex
	// tokenCache maps installation ID → the most recently fetched token.
	tokenCache map[string]cachedToken
	// tokenFlight deduplicates concurrent token-exchange requests for the
	// same installation ID, preventing a thundering-herd of GitHub API calls
	// when many goroutines notice an expired token at the same time.
	tokenFlight singleflight.Group
}

// NewGitHubAppClient creates a new GitHub App client
func NewGitHubAppClient(appID string, privateKeyPEM string) (*GitHubAppClient, error) {
	// Try to decode base64 private key first, fallback to raw PEM
	keyBytes := []byte(privateKeyPEM)
	decoded, err := base64.StdEncoding.DecodeString(privateKeyPEM)
	if err == nil {
		// Successfully decoded from base64
		keyBytes = decoded
	}
	// If base64 decode fails, use the raw string (it's already PEM format)

	// Parse RSA private key
	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	return &GitHubAppClient{
		AppID:      appID,
		PrivateKey: privateKey,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		UserAgent:  "grainlify-backend",
		BaseURL:    "https://api.github.com",
		Clock:      realClock{},
		tokenCache: make(map[string]cachedToken),
	}, nil
}

// GenerateJWT generates a JWT token for GitHub App authentication.
//
// The token is signed with RS256 and includes the following claims:
// - iat (issued at): Set to current time minus 60 seconds to account for clock skew
// - exp (expiration): Set to current time plus 10 minutes
// - iss (issuer): The GitHub App ID
//
// The 60-second clock skew offset ensures the token is accepted even if the
// server's clock is slightly ahead of the client's clock. The 10-minute
// expiration follows GitHub's requirements for App JWTs.
func (c *GitHubAppClient) GenerateJWT() (string, error) {
	now := c.Clock.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(), // Issued at time (allow 60s clock skew)
		"exp": now.Add(10 * time.Minute).Unix(),   // Expires in 10 minutes
		"iss": c.AppID,                            // Issuer is the App ID
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(c.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	return tokenString, nil
}

// InstallationTokenResponse represents the response from GitHub's installation token endpoint
type InstallationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GetInstallationToken returns a valid installation access token for the given
// installation ID.  Tokens are cached in memory and reused until they are
// within tokenRefreshAheadWindow of expiry (default: 5 minutes), at which
// point a new token is fetched from the GitHub API.
//
// Concurrent callers that all find the cached token stale are coalesced via
// a singleflight.Group so that only one token-exchange request is sent to
// GitHub, regardless of how many goroutines are waiting.  All waiters receive
// the same fresh token.
//
// If the token-exchange call fails, the error is returned directly; no stale
// token is silently reused, because an expired token would cause downstream
// GitHub API calls to fail with 401s.
func (c *GitHubAppClient) GetInstallationToken(ctx context.Context, installationID string) (string, error) {
	// Fast path: return the cached token if it is still valid.
	c.tokenCacheMu.Lock()
	if c.tokenCache == nil {
		c.tokenCache = make(map[string]cachedToken)
	}
	if ct, ok := c.tokenCache[installationID]; ok && ct.isValid(c.Clock.Now()) {
		c.tokenCacheMu.Unlock()
		return ct.token, nil
	}
	c.tokenCacheMu.Unlock()

	// Slow path: fetch a new token.  Use singleflight so that N concurrent
	// callers that all found the cache stale send exactly one HTTP request.
	type result struct {
		token     string
		expiresAt time.Time
	}
	v, err, _ := c.tokenFlight.Do(installationID, func() (interface{}, error) {
		// Re-check the cache inside the singleflight callback: a previous
		// waiter may have already populated it while we were waiting.
		c.tokenCacheMu.Lock()
		if ct, ok := c.tokenCache[installationID]; ok && ct.isValid(c.Clock.Now()) {
			c.tokenCacheMu.Unlock()
			return result{token: ct.token, expiresAt: ct.expiresAt}, nil
		}
		c.tokenCacheMu.Unlock()

		token, expiresAt, err := c.fetchInstallationToken(ctx, installationID)
		if err != nil {
			return nil, err
		}

		c.tokenCacheMu.Lock()
		c.tokenCache[installationID] = cachedToken{token: token, expiresAt: expiresAt}
		c.tokenCacheMu.Unlock()

		return result{token: token, expiresAt: expiresAt}, nil
	})
	if err != nil {
		return "", err
	}
	return v.(result).token, nil
}

// fetchInstallationToken performs the actual GitHub API call to exchange a
// signed App JWT for a short-lived installation access token.  It returns
// the token string and its expiry time so the caller can populate the cache.
func (c *GitHubAppClient) fetchInstallationToken(ctx context.Context, installationID string) (string, time.Time, error) {
	jwtToken, err := c.GenerateJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to generate JWT: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", c.BaseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		if resp.StatusCode == http.StatusNotFound {
			return "", time.Time{}, &InstallationNotFoundError{
				InstallationID: installationID,
				StatusCode:     resp.StatusCode,
			}
		}
		return "", time.Time{}, fmt.Errorf("failed to get installation token: status %d, error: %v", resp.StatusCode, errBody)
	}

	var tokenResp InstallationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", time.Time{}, err
	}

	return tokenResp.Token, tokenResp.ExpiresAt, nil
}

// InstallationRepository represents a repository in a GitHub App installation
type InstallationRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Owner    struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"` // "User" or "Organization"
	} `json:"owner"`
	Language    *string `json:"language"`
	Description *string `json:"description"`
	Topics      []string `json:"topics"`
}

// maxInstallationRepositoryPages is a safety cap to prevent runaway pagination.
const maxInstallationRepositoryPages = 20

// ListInstallationRepositories lists all repositories accessible to an installation.
// It fetches every page (per_page=100) and aggregates results. A safety cap of
// maxInstallationRepositoryPages prevents infinite pagination loops.
func (c *GitHubAppClient) ListInstallationRepositories(ctx context.Context, installationToken string) ([]InstallationRepository, error) {
	var all []InstallationRepository

	for page := 1; page <= maxInstallationRepositoryPages; page++ {
		u, _ := url.Parse(fmt.Sprintf("%s/installation/repositories", c.BaseURL))
		q := u.Query()
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("Authorization", "Bearer "+installationToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}

		var pageResult struct {
			TotalCount    int                      `json:"total_count"`
			Repositories  []InstallationRepository `json:"repositories"`
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				var errBody map[string]interface{}
				json.NewDecoder(resp.Body).Decode(&errBody)
				return fmt.Errorf("failed to list repositories: status %d, error: %v", resp.StatusCode, errBody)
			}
			return json.NewDecoder(resp.Body).Decode(&pageResult)
		}()
		if err != nil {
			return nil, err
		}

		all = append(all, pageResult.Repositories...)

		// Stop once we've accumulated everything the server reports via
		// total_count, or once a page comes back empty. Do NOT treat a
		// short page (fewer than per_page items) as "last page": GitHub's
		// total_count is the authoritative signal for this endpoint, and a
		// page can legitimately be shorter than per_page while more pages
		// remain.
		if len(pageResult.Repositories) == 0 || len(all) >= pageResult.TotalCount {
			break
		}

		if page == maxInstallationRepositoryPages {
			slog.Warn("installation repository pagination hit safety cap",
				"cap", maxInstallationRepositoryPages,
				"fetched", len(all),
				"total_count", pageResult.TotalCount,
			)
		}
	}

	return all, nil
}

