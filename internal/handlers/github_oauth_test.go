package handlers

import (
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

func TestIsAllowedRedirectURI_CORSAllowPreviewGating(t *testing.T) {
	previewOrigins := []string{
		"https://attacker-controlled.vercel.app",
		"https://my-preview-app.vercel.app/auth/callback",
		"https://subdomain.0xo.in",
		"https://test.0xo.in/callback",
	}

	t.Run("Rejects preview origins when CORSAllowPreview is false", func(t *testing.T) {
		cfg := config.Config{
			CORSAllowPreview: false,
		}

		for _, uri := range previewOrigins {
			if isAllowedRedirectURI(uri, cfg) {
				t.Errorf("expected isAllowedRedirectURI(%q, CORSAllowPreview=false) to be false, got true", uri)
			}
		}
	})

	t.Run("Accepts preview origins when CORSAllowPreview is true", func(t *testing.T) {
		cfg := config.Config{
			CORSAllowPreview: true,
		}

		for _, uri := range previewOrigins {
			if !isAllowedRedirectURI(uri, cfg) {
				t.Errorf("expected isAllowedRedirectURI(%q, CORSAllowPreview=true) to be true, got false", uri)
			}
		}
	})
}

func TestIsAllowedRedirectURI_RegressionDefaults(t *testing.T) {
	testCases := []struct {
		name string
		uri  string
		want bool
	}{
		{name: "Localhost HTTP with port", uri: "http://localhost:3000/auth/callback", want: true},
		{name: "Localhost HTTPS with port", uri: "https://localhost:8080", want: true},
		{name: "127.0.0.1 HTTP with port", uri: "http://127.0.0.1:3000", want: true},
		{name: "127.0.0.1 HTTPS with port", uri: "https://127.0.0.1:8080/callback", want: true},
		{name: "Explicit CORS origin match", uri: "https://app.grainlify.com/callback", want: true},
		{name: "FrontendBaseURL match", uri: "https://grainlify.com/auth/callback", want: true},
		{name: "Unallowed domain", uri: "https://attacker.com/oauth/callback", want: false},
		{name: "Domain substring spoofing", uri: "https://vercel.app.attacker.com", want: false},
		{name: "Invalid URL string", uri: "ht tp://invalid-url", want: false},
	}

	for _, flag := range []bool{false, true} {
		cfg := config.Config{
			CORSAllowPreview: flag,
			CORSOrigins:      "https://app.grainlify.com,https://dashboard.grainlify.com",
			FrontendBaseURL:  "https://grainlify.com",
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				got := isAllowedRedirectURI(tc.uri, cfg)
				if got != tc.want {
					t.Errorf("isAllowedRedirectURI(%q) with CORSAllowPreview=%v = %v, want %v", tc.uri, flag, got, tc.want)
				}
			})
		}
	}
}
