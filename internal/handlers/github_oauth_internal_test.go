package handlers

import (
	"encoding/base64"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

func TestIsAllowedRedirectURI(t *testing.T) {
	cfg := config.Config{
		CORSOrigins:     "https://allowed-cors.example.com,https://another-cors.example.com",
		FrontendBaseURL: "https://app.grainlify.com",
	}

	tests := []struct {
		name        string
		redirectURI string
		want        bool
	}{
		{"localhost with port 5173", "http://localhost:5173/auth/callback", true},
		{"localhost with port 3000", "http://localhost:3000/", true},
		{"127.0.0.1 with port 5173", "http://127.0.0.1:5173/auth/callback", true},
		{"127.0.0.1 with port 8080", "http://127.0.0.1:8080/", true},
		{"vercel preview deployment", "https://my-app-git-branch-team.vercel.app/auth/callback", true},
		{"first cors origin match", "https://allowed-cors.example.com/auth/callback", true},
		{"second cors origin match", "https://another-cors.example.com/some/path", true},
		{"frontend base url exact match", "https://app.grainlify.com/auth/callback", true},
		{"unrelated origin", "https://evil.example.com/auth/callback", false},
		{"unrelated origin similar to frontend base url", "https://notapp.grainlify.com.evil.com/x", false},
		{"malformed url with control byte", "\x7f", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowedRedirectURI(tt.redirectURI, cfg)
			if got != tt.want {
				t.Errorf("isAllowedRedirectURI(%q) = %v, want %v", tt.redirectURI, got, tt.want)
			}
		})
	}
}

func TestRandomState(t *testing.T) {
	s := randomState(32)
	wantLen := base64.RawURLEncoding.EncodedLen(32)
	if len(s) != wantLen {
		t.Errorf("len(randomState(32)) = %d, want %d", len(s), wantLen)
	}

	s2 := randomState(32)
	if s == s2 {
		t.Errorf("randomState(32) produced identical values on consecutive calls: %q", s)
	}
}

func TestEncodeDecodeStateWithRedirect_RoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		csrfToken   string
		redirectURI string
	}{
		{"with redirect", "csrf-token-abc123", "https://app.example.com/auth/callback"},
		{"redirect with query params", "csrf-token-qqq999", "https://app.example.com/callback?foo=bar&baz=qux"},
		// A csrfToken containing characters outside the base64url alphabet
		// (periods) guarantees base64 decoding of the raw token fails,
		// deterministically exercising the legacy (pre redirect_uri) code
		// path regardless of random byte content.
		{"empty redirect uses legacy (non-base64) csrf token", "fixed.csrf.token.xyz789", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeStateWithRedirect(tt.csrfToken, tt.redirectURI)
			gotCSRF, gotRedirect, err := decodeStateWithRedirect(encoded)
			if err != nil {
				t.Fatalf("decodeStateWithRedirect(%q) returned error: %v", encoded, err)
			}
			if gotCSRF != tt.csrfToken {
				t.Errorf("csrfToken = %q, want %q", gotCSRF, tt.csrfToken)
			}
			if gotRedirect != tt.redirectURI {
				t.Errorf("redirectURI = %q, want %q", gotRedirect, tt.redirectURI)
			}
		})
	}
}

// decodeStateWithRedirect never returns a non-nil error: by design, input
// that can't be parsed as csrf_token|redirect_uri is treated as a legacy
// (pre redirect_uri) raw CSRF token for backward compatibility, rather than
// being rejected outright. This test documents and locks in that actual
// behavior.
func TestDecodeStateWithRedirect_GarbageInputTreatedAsLegacyToken(t *testing.T) {
	garbage := "not-valid-base64!!!"
	csrf, redirect, err := decodeStateWithRedirect(garbage)
	if err != nil {
		t.Fatalf("decodeStateWithRedirect(%q) returned error = %v, want nil", garbage, err)
	}
	if csrf != garbage {
		t.Errorf("csrfToken = %q, want original garbage string %q", csrf, garbage)
	}
	if redirect != "" {
		t.Errorf("redirectURI = %q, want empty", redirect)
	}
}

func TestEffectiveGitHubRedirect(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			name: "explicit GitHubOAuthRedirectURL takes precedence over everything",
			cfg: config.Config{
				GitHubOAuthRedirectURL: "https://api.example.com/auth/github/login/callback",
				GitHubLoginRedirectURL: "https://legacy.example.com/callback",
				PublicBaseURL:          "https://api.example.com",
			},
			want: "https://api.example.com/auth/github/login/callback",
		},
		{
			name: "falls back to legacy GitHubLoginRedirectURL when GitHubOAuthRedirectURL is unset",
			cfg: config.Config{
				GitHubLoginRedirectURL: "https://legacy.example.com/callback",
				PublicBaseURL:          "https://api.example.com",
			},
			want: "https://legacy.example.com/callback",
		},
		{
			name: "derives from PublicBaseURL when neither redirect field is configured",
			cfg: config.Config{
				PublicBaseURL: "https://api.example.com/",
			},
			want: "https://api.example.com/auth/github/login/callback",
		},
		{
			name: "empty when nothing is configured",
			cfg:  config.Config{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveGitHubRedirect(tt.cfg)
			if got != tt.want {
				t.Errorf("effectiveGitHubRedirect() = %q, want %q", got, tt.want)
			}
		})
	}
}
