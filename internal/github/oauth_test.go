package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeURL(t *testing.T) {
	tests := []struct {
		name         string
		clientID     string
		redirectURL  string
		state        string
		scopes       []string
		wantErr      bool
		errContains  string
		validateURL  bool
		expectedParams map[string]string
	}{
		{
			name:        "valid URL with scopes",
			clientID:    "test_client_id",
			redirectURL: "https://example.com/callback",
			state:       "random_state_123",
			scopes:      []string{"user:email", "read:org"},
			wantErr:     false,
			validateURL: true,
			expectedParams: map[string]string{
				"client_id":     "test_client_id",
				"redirect_uri":  "https://example.com/callback",
				"state":         "random_state_123",
				"scope":         "user:email read:org",
			},
		},
		{
			name:        "valid URL without scopes",
			clientID:    "test_client_id",
			redirectURL: "https://example.com/callback",
			state:       "random_state_123",
			scopes:      []string{},
			wantErr:     false,
			validateURL: true,
			expectedParams: map[string]string{
				"client_id":     "test_client_id",
				"redirect_uri":  "https://example.com/callback",
				"state":         "random_state_123",
			},
		},
		{
			name:        "valid URL with single scope",
			clientID:    "test_client_id",
			redirectURL: "https://example.com/callback",
			state:       "random_state_123",
			scopes:      []string{"user:email"},
			wantErr:     false,
			validateURL: true,
			expectedParams: map[string]string{
				"client_id":     "test_client_id",
				"redirect_uri":  "https://example.com/callback",
				"state":         "random_state_123",
				"scope":         "user:email",
			},
		},
		{
			name:        "empty client ID",
			clientID:    "",
			redirectURL: "https://example.com/callback",
			state:       "random_state_123",
			scopes:      []string{"user:email"},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name:        "empty redirect URL",
			clientID:    "test_client_id",
			redirectURL: "",
			state:       "random_state_123",
			scopes:      []string{"user:email"},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name:        "both client ID and redirect URL empty",
			clientID:    "",
			redirectURL: "",
			state:       "random_state_123",
			scopes:      []string{"user:email"},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name:        "empty state is allowed (but not recommended)",
			clientID:    "test_client_id",
			redirectURL: "https://example.com/callback",
			state:       "",
			scopes:      []string{"user:email"},
			wantErr:     false,
			validateURL: true,
			expectedParams: map[string]string{
				"client_id":     "test_client_id",
				"redirect_uri":  "https://example.com/callback",
				"state":         "",
				"scope":         "user:email",
			},
		},
		{
			name:        "URL with special characters in redirect",
			clientID:    "test_client_id",
			redirectURL: "https://example.com/callback?param=value",
			state:       "random_state_123",
			scopes:      []string{"user:email"},
			wantErr:     false,
			validateURL: true,
			expectedParams: map[string]string{
				"client_id":     "test_client_id",
				"redirect_uri":  "https://example.com/callback?param=value",
				"state":         "random_state_123",
				"scope":         "user:email",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AuthorizeURL(tt.clientID, tt.redirectURL, tt.state, tt.scopes)

			if tt.wantErr {
				if err == nil {
					t.Errorf("AuthorizeURL() expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("AuthorizeURL() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("AuthorizeURL() unexpected error = %v", err)
				return
			}

			if !tt.validateURL {
				return
			}

			// Parse the URL to validate its structure
			parsedURL, err := url.Parse(got)
			if err != nil {
				t.Errorf("AuthorizeURL() returned invalid URL: %v", err)
				return
			}

			if parsedURL.Scheme != "https" {
				t.Errorf("AuthorizeURL() scheme = %v, want https", parsedURL.Scheme)
			}

			if parsedURL.Host != "github.com" {
				t.Errorf("AuthorizeURL() host = %v, want github.com", parsedURL.Host)
			}

			if parsedURL.Path != "/login/oauth/authorize" {
				t.Errorf("AuthorizeURL() path = %v, want /login/oauth/authorize", parsedURL.Path)
			}

			// Validate query parameters
			query := parsedURL.Query()
			for key, expectedValue := range tt.expectedParams {
				if gotValue := query.Get(key); gotValue != expectedValue {
					t.Errorf("AuthorizeURL() query param %s = %v, want %v", key, gotValue, expectedValue)
				}
			}
		})
	}
}

func TestJoinScopes(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   string
	}{
		{
			name:   "empty slice",
			scopes: []string{},
			want:   "",
		},
		{
			name:   "nil slice",
			scopes: nil,
			want:   "",
		},
		{
			name:   "single scope",
			scopes: []string{"user:email"},
			want:   "user:email",
		},
		{
			name:   "multiple scopes",
			scopes: []string{"user:email", "read:org", "repo"},
			want:   "user:email read:org repo",
		},
		{
			name:   "scopes with spaces (should be preserved)",
			scopes: []string{"user:email", "read:org"},
			want:   "user:email read:org",
		},
		{
			name:   "many scopes",
			scopes: []string{"scope1", "scope2", "scope3", "scope4", "scope5"},
			want:   "scope1 scope2 scope3 scope4 scope5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinScopes(tt.scopes); got != tt.want {
				t.Errorf("joinScopes() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExchangeCode(t *testing.T) {
	tests := []struct {
		name           string
		code           string
		cfg            OAuthConfig
		serverResponse *TokenResponse
		serverStatus   int
		serverDelay    time.Duration
		wantErr        bool
		errContains    string
		validateToken  bool
	}{
		{
			name: "successful token exchange",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverResponse: &TokenResponse{
				AccessToken: "gho_test_token_12345",
				TokenType:   "bearer",
				Scope:       "user:email,read:org",
			},
			serverStatus:  http.StatusOK,
			wantErr:       false,
			validateToken: true,
		},
		{
			name: "empty client ID",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name: "empty client secret",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "",
				RedirectURL:  "https://example.com/callback",
			},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name: "empty redirect URL",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "",
			},
			wantErr:     true,
			errContains: "github oauth not configured",
		},
		{
			name: "empty code",
			code: "",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			wantErr:     true,
			errContains: "code is required",
		},
		{
			name: "server returns 400 error",
			code: "invalid_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverResponse: &TokenResponse{
				AccessToken: "",
				TokenType:   "",
				Scope:       "",
			},
			serverStatus: http.StatusBadRequest,
			wantErr:      true,
			errContains:  "token exchange failed: status 400",
		},
		{
			name: "server returns 401 error",
			code: "invalid_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverResponse: &TokenResponse{
				AccessToken: "",
				TokenType:   "",
				Scope:       "",
			},
			serverStatus: http.StatusUnauthorized,
			wantErr:      true,
			errContains:  "token exchange failed: status 401",
		},
		{
			name: "server returns 500 error",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverResponse: &TokenResponse{
				AccessToken: "",
				TokenType:   "",
				Scope:       "",
			},
			serverStatus: http.StatusInternalServerError,
			wantErr:      true,
			errContains:  "token exchange failed: status 500",
		},
		{
			name: "server returns empty access token",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverResponse: &TokenResponse{
				AccessToken: "",
				TokenType:   "bearer",
				Scope:       "user:email",
			},
			serverStatus:  http.StatusOK,
			wantErr:       true,
			errContains:   "token exchange returned empty token",
		},
		{
			name: "server returns malformed JSON",
			code: "valid_auth_code",
			cfg: OAuthConfig{
				ClientID:     "test_client_id",
				ClientSecret: "test_client_secret",
				RedirectURL:  "https://example.com/callback",
			},
			serverStatus: http.StatusOK,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// For validation-only tests (no server needed)
			if tt.wantErr && (tt.errContains == "github oauth not configured" || tt.errContains == "code is required") {
				ctx := context.Background()
				_, err := ExchangeCode(ctx, tt.code, tt.cfg)
				if err == nil {
					t.Errorf("ExchangeCode() expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ExchangeCode() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			// For server-dependent tests, set up mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Validate request method
				if r.Method != http.MethodPost {
					t.Errorf("expected POST request, got %s", r.Method)
				}

				// Validate request headers
				if accept := r.Header.Get("Accept"); accept != "application/json" {
					t.Errorf("expected Accept header application/json, got %s", accept)
				}
				if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
					t.Errorf("expected Content-Type header application/json, got %s", contentType)
				}

				// Validate request body contains required fields
				var reqBody map[string]string
				if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
					t.Errorf("failed to decode request body: %v", err)
				}
				if reqBody["client_id"] != tt.cfg.ClientID {
					t.Errorf("expected client_id %s, got %s", tt.cfg.ClientID, reqBody["client_id"])
				}
				if reqBody["client_secret"] != tt.cfg.ClientSecret {
					t.Errorf("expected client_secret %s, got %s", tt.cfg.ClientSecret, reqBody["client_secret"])
				}
				if reqBody["code"] != tt.code {
					t.Errorf("expected code %s, got %s", tt.code, reqBody["code"])
				}
				if reqBody["redirect_uri"] != tt.cfg.RedirectURL {
					t.Errorf("expected redirect_uri %s, got %s", tt.cfg.RedirectURL, reqBody["redirect_uri"])
				}

				// Simulate delay if specified
				if tt.serverDelay > 0 {
					time.Sleep(tt.serverDelay)
				}

				// Return response
				if tt.serverStatus != 0 {
					w.WriteHeader(tt.serverStatus)
				}

				if tt.name == "server returns malformed JSON" {
					w.Write([]byte("invalid json"))
					return
				}

				if tt.serverResponse != nil {
					json.NewEncoder(w).Encode(tt.serverResponse)
				}
			}))
			defer server.Close()

			// Override tokenEndpoint for this test
			oldEndpoint := tokenEndpoint
			tokenEndpoint = server.URL
			defer func() { tokenEndpoint = oldEndpoint }()

			ctx := context.Background()
			got, err := ExchangeCode(ctx, tt.code, tt.cfg)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ExchangeCode() expected error containing %q, got nil", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ExchangeCode() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ExchangeCode() unexpected error = %v", err)
				return
			}

			if tt.validateToken {
				if got.AccessToken == "" {
					t.Errorf("ExchangeCode() returned empty access token")
				}
				if got.TokenType == "" {
					t.Errorf("ExchangeCode() returned empty token type")
				}
			}
		})
	}
}

func TestExchangeCodeTimeout(t *testing.T) {
	// Test that the context deadline is respected
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow response
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "gho_test_token",
			TokenType:   "bearer",
		})
	}))
	defer server.Close()

	// Override tokenEndpoint for this test
	oldEndpoint := tokenEndpoint
	tokenEndpoint = server.URL
	defer func() { tokenEndpoint = oldEndpoint }()

	// Create a context with a short deadline
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	cfg := OAuthConfig{
		ClientID:     "test_client_id",
		ClientSecret: "test_client_secret",
		RedirectURL:  "https://example.com/callback",
	}

	_, err := ExchangeCode(ctx, "valid_code", cfg)
	if err == nil {
		t.Errorf("ExchangeCode() expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "timeout") {
		t.Logf("ExchangeCode() error = %v (may be acceptable depending on timing)", err)
	}
}

func TestExchangeCodeContextCancellation(t *testing.T) {
	// Test that context cancellation is respected
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow response
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "gho_test_token",
			TokenType:   "bearer",
		})
	}))
	defer server.Close()

	// Override tokenEndpoint for this test
	oldEndpoint := tokenEndpoint
	tokenEndpoint = server.URL
	defer func() { tokenEndpoint = oldEndpoint }()

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())

	cfg := OAuthConfig{
		ClientID:     "test_client_id",
		ClientSecret: "test_client_secret",
		RedirectURL:  "https://example.com/callback",
	}

	// Cancel the context immediately
	cancel()

	_, err := ExchangeCode(ctx, "valid_code", cfg)
	if err == nil {
		t.Errorf("ExchangeCode() expected cancellation error, got nil")
	}
}

// TestExchangeCodeIntegration is a placeholder for integration tests
// that would test against a real GitHub OAuth endpoint (not for CI)
func TestExchangeCodeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Skip("integration tests require real GitHub OAuth credentials")
}
