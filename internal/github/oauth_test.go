package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Tests in this file mutate the package-level tokenEndpoint var to point at
// an httptest.Server, so they must not run with t.Parallel().

func TestExchangeCode_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q, want application/json", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type header = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"gho_abc123token","token_type":"bearer","scope":"read:user,user:email"}`))
	}))
	defer server.Close()

	orig := tokenEndpoint
	tokenEndpoint = server.URL
	t.Cleanup(func() { tokenEndpoint = orig })

	cfg := OAuthConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/auth/github/login/callback",
	}
	tr, err := ExchangeCode(context.Background(), "test-code", cfg)
	if err != nil {
		t.Fatalf("ExchangeCode returned error: %v", err)
	}
	if tr.AccessToken != "gho_abc123token" {
		t.Errorf("AccessToken = %q, want %q", tr.AccessToken, "gho_abc123token")
	}
	if tr.TokenType != "bearer" {
		t.Errorf("TokenType = %q, want %q", tr.TokenType, "bearer")
	}
	if tr.Scope != "read:user,user:email" {
		t.Errorf("Scope = %q, want %q", tr.Scope, "read:user,user:email")
	}
}

func TestExchangeCode_NonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
	}))
	defer server.Close()

	orig := tokenEndpoint
	tokenEndpoint = server.URL
	t.Cleanup(func() { tokenEndpoint = orig })

	cfg := OAuthConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/auth/github/login/callback",
	}
	_, err := ExchangeCode(context.Background(), "test-code", cfg)
	if err == nil {
		t.Fatal("ExchangeCode() error = nil, want error for non-2xx status")
	}
}

func TestExchangeCode_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token": not-valid-json`))
	}))
	defer server.Close()

	orig := tokenEndpoint
	tokenEndpoint = server.URL
	t.Cleanup(func() { tokenEndpoint = orig })

	cfg := OAuthConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/auth/github/login/callback",
	}
	_, err := ExchangeCode(context.Background(), "test-code", cfg)
	if err == nil {
		t.Fatal("ExchangeCode() error = nil, want error for malformed JSON body")
	}
}

func TestExchangeCode_EmptyAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"","token_type":"bearer","scope":""}`))
	}))
	defer server.Close()

	orig := tokenEndpoint
	tokenEndpoint = server.URL
	t.Cleanup(func() { tokenEndpoint = orig })

	cfg := OAuthConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost:8080/auth/github/login/callback",
	}
	_, err := ExchangeCode(context.Background(), "test-code", cfg)
	if err == nil {
		t.Fatal("ExchangeCode() error = nil, want error for empty access_token in a 200 response")
	}
}

func TestExchangeCode_MissingConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  OAuthConfig
	}{
		{"missing client id", OAuthConfig{ClientSecret: "s", RedirectURL: "r"}},
		{"missing client secret", OAuthConfig{ClientID: "c", RedirectURL: "r"}},
		{"missing redirect url", OAuthConfig{ClientID: "c", ClientSecret: "s"}},
		{"all empty", OAuthConfig{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExchangeCode(context.Background(), "test-code", tt.cfg)
			if err == nil {
				t.Fatal("ExchangeCode() error = nil, want error for incomplete OAuthConfig")
			}
		})
	}
}

func TestExchangeCode_MissingCode(t *testing.T) {
	cfg := OAuthConfig{ClientID: "c", ClientSecret: "s", RedirectURL: "r"}
	_, err := ExchangeCode(context.Background(), "", cfg)
	if err == nil {
		t.Fatal("ExchangeCode() error = nil, want error for empty code")
	}
}

func TestAuthorizeURL(t *testing.T) {
	got, err := AuthorizeURL("test-client-id", "http://localhost:8080/callback", "test-state-value", []string{"read:user", "user:email", "repo"})
	if err != nil {
		t.Fatalf("AuthorizeURL returned error: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthorizeURL() = %q, doesn't parse as URL: %v", got, err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https", u.Scheme)
	}
	if u.Host != "github.com" {
		t.Errorf("host = %q, want github.com", u.Host)
	}
	if u.Path != "/login/oauth/authorize" {
		t.Errorf("path = %q, want /login/oauth/authorize", u.Path)
	}

	q := u.Query()
	if q.Get("client_id") != "test-client-id" {
		t.Errorf("client_id = %q, want test-client-id", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://localhost:8080/callback" {
		t.Errorf("redirect_uri = %q, want http://localhost:8080/callback", q.Get("redirect_uri"))
	}
	if q.Get("state") != "test-state-value" {
		t.Errorf("state = %q, want test-state-value", q.Get("state"))
	}
	if want := "read:user user:email repo"; q.Get("scope") != want {
		t.Errorf("scope = %q, want %q (space-joined)", q.Get("scope"), want)
	}
}

func TestAuthorizeURL_NoScopes(t *testing.T) {
	got, err := AuthorizeURL("test-client-id", "http://localhost:8080/callback", "test-state-value", nil)
	if err != nil {
		t.Fatalf("AuthorizeURL returned error: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthorizeURL() = %q, doesn't parse as URL: %v", got, err)
	}
	if _, present := u.Query()["scope"]; present {
		t.Error("scope query param present, want absent when no scopes are given")
	}
}

func TestAuthorizeURL_MissingClientID(t *testing.T) {
	_, err := AuthorizeURL("", "http://localhost:8080/callback", "state", nil)
	if err == nil {
		t.Fatal("AuthorizeURL() error = nil, want error for empty clientID")
	}
}

func TestAuthorizeURL_MissingRedirectURL(t *testing.T) {
	_, err := AuthorizeURL("test-client-id", "", "state", nil)
	if err == nil {
		t.Fatal("AuthorizeURL() error = nil, want error for empty redirectURL")
	}
}
