package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests in this file mutate the package-level userAPIURL var to point at an
// httptest.Server, so they must not run with t.Parallel().

func TestGetUser_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-access-token")
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("User-Agent"); got != "patchwork-backend" {
			t.Errorf("User-Agent header = %q, want patchwork-backend", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":12345,"login":"octocat","avatar_url":"https://avatars.example/octocat.png","name":"The Octocat","email":"octocat@example.com"}`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	c := NewClient()
	u, err := c.GetUser(context.Background(), "test-access-token")
	if err != nil {
		t.Fatalf("GetUser returned error: %v", err)
	}
	if u.ID != 12345 {
		t.Errorf("ID = %d, want 12345", u.ID)
	}
	if u.Login != "octocat" {
		t.Errorf("Login = %q, want octocat", u.Login)
	}
	if u.AvatarURL != "https://avatars.example/octocat.png" {
		t.Errorf("AvatarURL = %q, want https://avatars.example/octocat.png", u.AvatarURL)
	}
	if u.Name != "The Octocat" {
		t.Errorf("Name = %q, want The Octocat", u.Name)
	}
}

func TestGetUser_NonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	c := NewClient()
	_, err := c.GetUser(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error for non-2xx status")
	}
}

func TestGetUser_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": not-valid-json`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	c := NewClient()
	_, err := c.GetUser(context.Background(), "test-token")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error for malformed JSON body")
	}
}

func TestGetUser_ZeroID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":0,"login":"octocat"}`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	c := NewClient()
	_, err := c.GetUser(context.Background(), "test-token")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error when id is 0")
	}
}

func TestGetUser_EmptyLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":12345,"login":""}`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	c := NewClient()
	_, err := c.GetUser(context.Background(), "test-token")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error when login is empty")
	}
}
