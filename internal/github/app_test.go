package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockClock allows controlling time in tests
type mockClock struct {
	now time.Time
}

func (m mockClock) Now() time.Time {
	return m.now
}

// generateTestRSAKey creates a fresh RSA key pair for testing
func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate RSA key: %v", err)
	}
	return privateKey, &privateKey.PublicKey
}

// TestGenerateJWT_Success tests that GenerateJWT produces a valid RS256 token
func TestGenerateJWT_Success(t *testing.T) {
	privateKey, publicKey := generateTestRSAKey(t)

	testTime := time.Now().UTC()
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		Clock:      mockClock{now: testTime},
	}

	tokenString, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	// Parse and verify the token
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method is RS256
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return publicKey, nil
	}, jwt.WithTimeFunc(func() time.Time { return testTime }))

	if err != nil {
		t.Fatalf("Failed to parse token: %v", err)
	}

	if !token.Valid {
		t.Fatal("Token is not valid")
	}

	// Verify claims
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("Claims are not of type MapClaims")
	}

	// Check iss (issuer)
	if iss, ok := claims["iss"].(string); !ok || iss != "test-app-id" {
		t.Errorf("Expected iss to be 'test-app-id', got %v", claims["iss"])
	}

	// Check iat (issued at) - should be testTime minus 60 seconds
	expectedIat := testTime.Add(-60 * time.Second).Unix()
	if iat, ok := claims["iat"].(float64); !ok || int64(iat) != expectedIat {
		t.Errorf("Expected iat to be %d, got %v", expectedIat, claims["iat"])
	}

	// Check exp (expiration) - should be testTime plus 10 minutes
	expectedExp := testTime.Add(10 * time.Minute).Unix()
	if exp, ok := claims["exp"].(float64); !ok || int64(exp) != expectedExp {
		t.Errorf("Expected exp to be %d, got %v", expectedExp, claims["exp"])
	}
}

// TestGenerateJWT_RS256Enforcement tests that the token uses RS256 signing
func TestGenerateJWT_RS256Enforcement(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		Clock:      mockClock{now: time.Now()},
	}

	tokenString, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	// Parse the token without verification to check the header
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		t.Fatalf("Invalid token format")
	}

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("Failed to decode header: %v", err)
	}

	var headerMap map[string]interface{}
	if err := json.Unmarshal(header, &headerMap); err != nil {
		t.Fatalf("Failed to unmarshal header: %v", err)
	}

	alg, ok := headerMap["alg"].(string)
	if !ok || alg != "RS256" {
		t.Errorf("Expected alg to be 'RS256', got %v", headerMap["alg"])
	}
}

// TestGenerateJWT_VerifyWithPublicKey tests that the token can be verified with the public key
func TestGenerateJWT_VerifyWithPublicKey(t *testing.T) {
	privateKey, publicKey := generateTestRSAKey(t)
	
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		Clock:      mockClock{now: time.Now()},
	}

	tokenString, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	// Verify with public key
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return publicKey, nil
	})

	if err != nil {
		t.Fatalf("Failed to verify token with public key: %v", err)
	}

	if !token.Valid {
		t.Fatal("Token verification failed")
	}
}

// TestGenerateJWT_RejectWrongSigningMethod tests that tokens with wrong signing method are rejected
func TestGenerateJWT_RejectWrongSigningMethod(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		Clock:      mockClock{now: time.Now()},
	}

	tokenString, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	// Try to verify with wrong signing method (e.g., HS256)
	_, err = jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// This will fail because we're returning RSA public key but the parser expects HMAC
		return []byte("wrong-key"), nil
	})

	if err == nil {
		t.Fatal("Expected error when verifying with wrong signing method")
	}
}

// TestGenerateJWT_Timing tests that the timing claims are correct
func TestGenerateJWT_Timing(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	// Test with a specific time
	testTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		Clock:      mockClock{now: testTime},
	}

	tokenString, err := client.GenerateJWT()
	if err != nil {
		t.Fatalf("GenerateJWT failed: %v", err)
	}

	token, _ := jwt.Parse(tokenString, nil, jwt.WithoutClaimsValidation())
	claims := token.Claims.(jwt.MapClaims)

	// Verify 60-second clock skew offset
	iat := int64(claims["iat"].(float64))
	expectedIat := testTime.Add(-60 * time.Second).Unix()
	if iat != expectedIat {
		t.Errorf("Clock skew offset incorrect: expected %d, got %d", expectedIat, iat)
	}

	// Verify 10-minute expiration
	exp := int64(claims["exp"].(float64))
	expectedExp := testTime.Add(10 * time.Minute).Unix()
	if exp != expectedExp {
		t.Errorf("Expiration incorrect: expected %d, got %d", expectedExp, exp)
	}

	// Verify the time window is exactly 10 minutes + 60 seconds skew
	timeWindow := exp - iat
	expectedWindow := int64((10 * time.Minute + 60 * time.Second).Seconds())
	if timeWindow != expectedWindow {
		t.Errorf("Time window incorrect: expected %d seconds, got %d seconds", expectedWindow, timeWindow)
	}
}

// TestGetInstallationToken_Success tests successful installation token retrieval
func TestGetInstallationToken_Success(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST request, got %s", r.Method)
		}

		// Verify Authorization header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("Expected Bearer token in Authorization header, got %s", auth)
		}

		// Verify Accept header
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Expected Accept header to be 'application/vnd.github+json', got %s", r.Header.Get("Accept"))
		}

		// Return mock response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_testInstallationToken",
			"expires_at": "2024-01-01T13:00:00Z",
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		HTTP:       server.Client(),
		UserAgent:  "test-user-agent",
		BaseURL:    server.URL,
		Clock:      mockClock{now: time.Now()},
	}

	token, err := client.GetInstallationToken(context.Background(), "12345")
	if err != nil {
		t.Fatalf("GetInstallationToken failed: %v", err)
	}

	if token != "ghs_testInstallationToken" {
		t.Errorf("Expected token 'ghs_testInstallationToken', got %s", token)
	}
}

// TestGetInstallationToken_ServerError tests error handling when server returns error
func TestGetInstallationToken_ServerError(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Bad credentials",
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		HTTP:       server.Client(),
		BaseURL:    server.URL,
		Clock:      mockClock{now: time.Now()},
	}

	_, err := client.GetInstallationToken(context.Background(), "12345")
	if err == nil {
		t.Fatal("Expected error when server returns 401")
	}

	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Expected error to contain '401', got %v", err)
	}
}

// TestGetInstallationToken_NetworkError tests error handling when network fails
func TestGetInstallationToken_NetworkError(t *testing.T) {
	privateKey, _ := generateTestRSAKey(t)
	
	client := &GitHubAppClient{
		AppID:      "test-app-id",
		PrivateKey: privateKey,
		HTTP:       &http.Client{},
		BaseURL:    "http://invalid-url-that-does-not-exist.local",
		Clock:      mockClock{now: time.Now()},
	}

	_, err := client.GetInstallationToken(context.Background(), "12345")
	if err == nil {
		t.Fatal("Expected error when network fails")
	}
}

// TestListInstallationRepositories_Success tests successful repository listing
func TestListInstallationRepositories_Success(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method
		if r.Method != http.MethodGet {
			t.Errorf("Expected GET request, got %s", r.Method)
		}

		// Verify Authorization header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("Expected Bearer token in Authorization header, got %s", auth)
		}

		// Return mock response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": []interface{}{
				map[string]interface{}{
					"id":        int64(12345),
					"full_name": "test-owner/test-repo",
					"name":      "test-repo",
					"private":   true,
					"owner": map[string]interface{}{
						"id":    int64(67890),
						"login": "test-owner",
						"type":  "Organization",
					},
					"language":    strPtr("Go"),
					"description": strPtr("Test repository"),
					"topics":      []string{"github", "app"},
				},
			},
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:      server.Client(),
		UserAgent: "test-user-agent",
		BaseURL:   server.URL,
	}

	repos, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err != nil {
		t.Fatalf("ListInstallationRepositories failed: %v", err)
	}

	if len(repos) != 1 {
		t.Fatalf("Expected 1 repository, got %d", len(repos))
	}

	repo := repos[0]
	if repo.ID != 12345 {
		t.Errorf("Expected repo ID 12345, got %d", repo.ID)
	}
	if repo.FullName != "test-owner/test-repo" {
		t.Errorf("Expected full_name 'test-owner/test-repo', got %s", repo.FullName)
	}
	if repo.Name != "test-repo" {
		t.Errorf("Expected name 'test-repo', got %s", repo.Name)
	}
	if !repo.Private {
		t.Error("Expected repo to be private")
	}
	if repo.Owner.ID != 67890 {
		t.Errorf("Expected owner ID 67890, got %d", repo.Owner.ID)
	}
	if repo.Owner.Login != "test-owner" {
		t.Errorf("Expected owner login 'test-owner', got %s", repo.Owner.Login)
	}
	if repo.Owner.Type != "Organization" {
		t.Errorf("Expected owner type 'Organization', got %s", repo.Owner.Type)
	}
	if repo.Language == nil || *repo.Language != "Go" {
		t.Errorf("Expected language 'Go', got %v", repo.Language)
	}
	if repo.Description == nil || *repo.Description != "Test repository" {
		t.Errorf("Expected description 'Test repository', got %v", repo.Description)
	}
	if len(repo.Topics) != 2 || repo.Topics[0] != "github" || repo.Topics[1] != "app" {
		t.Errorf("Expected topics ['github', 'app'], got %v", repo.Topics)
	}
}

// TestListInstallationRepositories_MultipleRepos tests parsing multiple repositories
func TestListInstallationRepositories_MultipleRepos(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": []interface{}{
				map[string]interface{}{
					"id":        int64(1),
					"full_name": "owner/repo1",
					"name":      "repo1",
					"private":   false,
					"owner": map[string]interface{}{
						"id":    int64(100),
						"login": "owner",
						"type":  "User",
					},
				},
				map[string]interface{}{
					"id":        int64(2),
					"full_name": "owner/repo2",
					"name":      "repo2",
					"private":   true,
					"owner": map[string]interface{}{
						"id":    int64(100),
						"login": "owner",
						"type":  "User",
					},
				},
			},
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	repos, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err != nil {
		t.Fatalf("ListInstallationRepositories failed: %v", err)
	}

	if len(repos) != 2 {
		t.Fatalf("Expected 2 repositories, got %d", len(repos))
	}

	if repos[0].ID != 1 || repos[1].ID != 2 {
		t.Errorf("Expected repo IDs [1, 2], got [%d, %d]", repos[0].ID, repos[1].ID)
	}
}

// TestListInstallationRepositories_ServerError tests error handling
func TestListInstallationRepositories_ServerError(t *testing.T) {
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Resource not accessible",
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	_, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err == nil {
		t.Fatal("Expected error when server returns 403")
	}

	if !strings.Contains(err.Error(), "403") {
		t.Errorf("Expected error to contain '403', got %v", err)
	}
}

// TestListInstallationRepositories_EmptyList tests handling empty repository list
func TestListInstallationRepositories_EmptyList(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"repositories": []interface{}{},
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	repos, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err != nil {
		t.Fatalf("ListInstallationRepositories failed: %v", err)
	}

	if len(repos) != 0 {
		t.Errorf("Expected 0 repositories, got %d", len(repos))
	}
}

// TestListInstallationRepositories_Pagination tests multi-page aggregation
func TestListInstallationRepositories_Pagination(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		page := r.URL.Query().Get("page")

		// Verify per_page=100
		if pp := r.URL.Query().Get("per_page"); pp != "100" {
			t.Errorf("Expected per_page=100, got %s", pp)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch page {
		case "1":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 3,
				"repositories": []interface{}{
					map[string]interface{}{"id": int64(1), "full_name": "o/r1", "name": "r1", "private": false, "owner": map[string]interface{}{"id": int64(10), "login": "o", "type": "User"}},
					map[string]interface{}{"id": int64(2), "full_name": "o/r2", "name": "r2", "private": false, "owner": map[string]interface{}{"id": int64(10), "login": "o", "type": "User"}},
				},
			})
		case "2":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 3,
				"repositories": []interface{}{
					map[string]interface{}{"id": int64(3), "full_name": "o/r3", "name": "r3", "private": false, "owner": map[string]interface{}{"id": int64(10), "login": "o", "type": "User"}},
				},
			})
		default:
			t.Errorf("Unexpected page request: %s", page)
			json.NewEncoder(w).Encode(map[string]interface{}{"total_count": 3, "repositories": []interface{}{}})
		}
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	repos, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err != nil {
		t.Fatalf("ListInstallationRepositories failed: %v", err)
	}

	if len(repos) != 3 {
		t.Fatalf("Expected 3 repositories across pages, got %d", len(repos))
	}

	if repos[0].ID != 1 || repos[1].ID != 2 || repos[2].ID != 3 {
		t.Errorf("Expected repo IDs [1, 2, 3], got [%d, %d, %d]", repos[0].ID, repos[1].ID, repos[2].ID)
	}

	if callCount != 2 {
		t.Errorf("Expected 2 HTTP requests, got %d", callCount)
	}
}

// TestListInstallationRepositories_SafetyCap verifies pagination stops at the cap
func TestListInstallationRepositories_SafetyCap(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Always return a full page with total_count larger than fetched,
		// so the loop would never stop without the cap.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 9999,
			"repositories": []interface{}{
				map[string]interface{}{"id": int64(callCount), "full_name": "o/r", "name": "r", "private": false, "owner": map[string]interface{}{"id": int64(10), "login": "o", "type": "User"}},
			},
		})
	}))
	defer server.Close()

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	repos, err := client.ListInstallationRepositories(context.Background(), "ghs_testToken")
	if err != nil {
		t.Fatalf("ListInstallationRepositories failed: %v", err)
	}

	if callCount != maxInstallationRepositoryPages {
		t.Errorf("Expected %d requests (safety cap), got %d", maxInstallationRepositoryPages, callCount)
	}

	if len(repos) != maxInstallationRepositoryPages {
		t.Errorf("Expected %d repositories (one per capped page), got %d", maxInstallationRepositoryPages, len(repos))
	}
}

// TestListInstallationRepositories_ContextCancellation tests that context is respected
func TestListInstallationRepositories_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"repositories": []interface{}{
				map[string]interface{}{"id": int64(1), "full_name": "o/r", "name": "r", "private": false, "owner": map[string]interface{}{"id": int64(10), "login": "o", "type": "User"}},
			},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	client := &GitHubAppClient{
		HTTP:    server.Client(),
		BaseURL: server.URL,
	}

	_, err := client.ListInstallationRepositories(ctx, "ghs_testToken")
	if err == nil {
		t.Fatal("Expected error from cancelled context")
	}
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}

// ---------------------------------------------------------------------------
// Installation-token caching tests
// ---------------------------------------------------------------------------
//
// These tests exercise the four observable behaviours of the cache layer that
// lives inside GetInstallationToken:
//
//   1. Cache HIT  – a valid, non-expiring-soon cached token is returned
//                   without making any HTTP request.
//   2. Cache MISS / expiry – an absent or expired token triggers exactly one
//                   HTTP round-trip; the fresh token is returned.
//   3. Refresh-ahead window – a token whose expiry is within
//                   tokenRefreshAheadWindow (5 min) of "now" is treated as
//                   stale and exchanged, even though it hasn't expired yet.
//   4. Stampede prevention – N concurrent callers that all observe a stale
//                   cache coalesce into exactly ONE GitHub API request.
//   5. Refresh failure – when the GitHub API returns an error the caller
//                   receives a clear error; no stale/expired token is silently
//                   returned.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newTokenExpiresAt returns the string form of t.Add(d) suitable for use as
// the "expires_at" field in a mock GitHub response body.
func newTokenExpiresAt(t *testing.T, base time.Time, d time.Duration) string {
	t.Helper()
	return base.Add(d).UTC().Format(time.RFC3339)
}

// buildTokenClient creates a GitHubAppClient whose HTTP transport points at
// the provided httptest.Server and whose Clock is fixed at clockNow.
func buildTokenClient(t *testing.T, server *httptest.Server, clockNow time.Time) *GitHubAppClient {
	t.Helper()
	privateKey, _ := generateTestRSAKey(t)
	return &GitHubAppClient{
		AppID:      "test-app",
		PrivateKey: privateKey,
		HTTP:       server.Client(),
		UserAgent:  "test",
		BaseURL:    server.URL,
		Clock:      mockClock{now: clockNow},
		tokenCache: make(map[string]cachedToken),
	}
}

// tokenResponse writes a standard GitHub installation-token JSON response.
func tokenResponse(w http.ResponseWriter, token, expiresAt string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":      token,
		"expires_at": expiresAt,
	})
}

// ---------------------------------------------------------------------------
// 1. Cache hit
// ---------------------------------------------------------------------------

// TestGetInstallationToken_CacheHit asserts that a valid, non-expiring-soon
// cached token is returned without performing any HTTP request.
func TestGetInstallationToken_CacheHit(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// The server should never be called; fail loudly if it is.
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		t.Errorf("unexpected HTTP call to token endpoint (cache should have served the request)")
		tokenResponse(w, "ghs_should_not_be_called", newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	// Pre-populate the cache with a token that expires 30 minutes from now —
	// well outside the 5-minute refresh-ahead window.
	cachedTok := "ghs_cached_valid_token"
	client.tokenCacheMu.Lock()
	client.tokenCache["inst-1"] = cachedToken{
		token:     cachedTok,
		expiresAt: now.Add(30 * time.Minute),
	}
	client.tokenCacheMu.Unlock()

	got, err := client.GetInstallationToken(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != cachedTok {
		t.Errorf("expected cached token %q, got %q", cachedTok, got)
	}
	if callCount != 0 {
		t.Errorf("expected 0 HTTP calls, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// 2. Cache miss – no entry yet
// ---------------------------------------------------------------------------

// TestGetInstallationToken_CacheMiss asserts that when there is no cached
// token exactly one HTTP request is made and the returned token is stored.
func TestGetInstallationToken_CacheMiss(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		tokenResponse(w, "ghs_fresh_token", newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	got, err := client.GetInstallationToken(context.Background(), "inst-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghs_fresh_token" {
		t.Errorf("expected %q, got %q", "ghs_fresh_token", got)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 HTTP call, got %d", callCount)
	}

	// The token should now be in the cache.
	client.tokenCacheMu.Lock()
	ct := client.tokenCache["inst-2"]
	client.tokenCacheMu.Unlock()
	if ct.token != "ghs_fresh_token" {
		t.Errorf("cache not populated: expected %q, got %q", "ghs_fresh_token", ct.token)
	}
}

// ---------------------------------------------------------------------------
// 3a. Expired token → refresh
// ---------------------------------------------------------------------------

// TestGetInstallationToken_ExpiredToken asserts that a token whose expiry has
// already passed (hard-expired) triggers a fresh API call.
func TestGetInstallationToken_ExpiredToken(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		tokenResponse(w, "ghs_refreshed_token", newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	// Inject an already-expired token.
	client.tokenCacheMu.Lock()
	client.tokenCache["inst-3"] = cachedToken{
		token:     "ghs_expired_token",
		expiresAt: now.Add(-10 * time.Minute), // expired 10 minutes ago
	}
	client.tokenCacheMu.Unlock()

	got, err := client.GetInstallationToken(context.Background(), "inst-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghs_refreshed_token" {
		t.Errorf("expected refreshed token %q, got %q", "ghs_refreshed_token", got)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 HTTP call, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// 3b. Refresh-ahead window
// ---------------------------------------------------------------------------

// TestGetInstallationToken_RefreshAheadWindow asserts that the refresh-ahead
// window is exactly tokenRefreshAheadWindow (5 minutes).
//
// Tokens are refreshed when:   now >= expiresAt - tokenRefreshAheadWindow
// i.e. when remaining TTL < tokenRefreshAheadWindow.
//
// We test three boundary points:
//   - remaining = tokenRefreshAheadWindow + 1s  →  cache hit  (still valid)
//   - remaining = tokenRefreshAheadWindow        →  boundary, treated as stale
//   - remaining = tokenRefreshAheadWindow - 1s   →  definitely stale, refresh
func TestGetInstallationToken_RefreshAheadWindow(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		remaining   time.Duration // time until token expires relative to now
		expectFetch bool          // true if we expect an HTTP call
	}{
		{
			name:        "well_within_window_no_refresh",
			remaining:   tokenRefreshAheadWindow + time.Second,
			expectFetch: false,
		},
		{
			name:        "at_boundary_stale",
			remaining:   tokenRefreshAheadWindow,
			expectFetch: true,
		},
		{
			name:        "inside_window_refresh",
			remaining:   tokenRefreshAheadWindow - time.Second,
			expectFetch: true,
		},
		{
			name:        "nearly_expired_refresh",
			remaining:   30 * time.Second,
			expectFetch: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			callCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callCount++
				tokenResponse(w, "ghs_new_token", newTokenExpiresAt(t, now, time.Hour))
			}))
			defer server.Close()

			client := buildTokenClient(t, server, now)

			// Seed the cache with a token that has the desired remaining TTL.
			client.tokenCacheMu.Lock()
			client.tokenCache["inst-window"] = cachedToken{
				token:     "ghs_old_token",
				expiresAt: now.Add(tc.remaining),
			}
			client.tokenCacheMu.Unlock()

			tok, err := client.GetInstallationToken(context.Background(), "inst-window")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.expectFetch {
				if callCount != 1 {
					t.Errorf("expected 1 HTTP call (refresh), got %d", callCount)
				}
				if tok != "ghs_new_token" {
					t.Errorf("expected fresh token, got %q", tok)
				}
			} else {
				if callCount != 0 {
					t.Errorf("expected 0 HTTP calls (cache hit), got %d", callCount)
				}
				if tok != "ghs_old_token" {
					t.Errorf("expected cached token, got %q", tok)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Stampede prevention (concurrent callers)
// ---------------------------------------------------------------------------

// TestGetInstallationToken_ConcurrentStampedePreventiontest verifies that N
// concurrent goroutines that all find an empty cache coalesce into exactly ONE
// HTTP call to the GitHub token endpoint (singleflight deduplication).
func TestGetInstallationToken_ConcurrentStampedePreventiontest(t *testing.T) {
	const numCallers = 10

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// Use an atomic counter to count how many times the handler is invoked.
	var apiCallCount int64
	// Barrier to make all goroutines hit the client at the same moment.
	var ready sync.WaitGroup
	var release = make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiCallCount, 1)
		// Simulate a modest network delay so concurrent goroutines have time
		// to queue up inside singleflight before the first one returns.
		time.Sleep(20 * time.Millisecond)
		tokenResponse(w, "ghs_singleton_token", newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	results := make([]string, numCallers)
	errs := make([]error, numCallers)
	ready.Add(numCallers)

	var wg sync.WaitGroup
	wg.Add(numCallers)
	for i := 0; i < numCallers; i++ {
		i := i
		go func() {
			defer wg.Done()
			ready.Done()
			<-release // wait for the signal to start simultaneously
			results[i], errs[i] = client.GetInstallationToken(context.Background(), "inst-stampede")
		}()
	}

	// Wait until all goroutines are parked, then release them together.
	ready.Wait()
	close(release)
	wg.Wait()

	// All callers must have received the token without error.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d got error: %v", i, err)
		}
	}
	for i, tok := range results {
		if tok != "ghs_singleton_token" {
			t.Errorf("goroutine %d got unexpected token %q", i, tok)
		}
	}

	// The critical assertion: only ONE API call must have reached the server.
	if n := atomic.LoadInt64(&apiCallCount); n != 1 {
		t.Errorf("singleflight failed: expected 1 API call, got %d", n)
	}
}

// TestGetInstallationToken_ConcurrentDifferentInstallations asserts that
// concurrent refreshes for DIFFERENT installation IDs each get their own
// API call (singleflight key is per installation ID).
func TestGetInstallationToken_ConcurrentDifferentInstallations(t *testing.T) {
	const numInstallations = 5

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	var apiCallCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiCallCount, 1)
		// Extract installation ID from path to return a distinct token.
		parts := strings.Split(r.URL.Path, "/")
		instID := parts[len(parts)-2]
		time.Sleep(10 * time.Millisecond)
		tokenResponse(w, "ghs_token_"+instID, newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	var wg sync.WaitGroup
	results := make([]string, numInstallations)
	errs := make([]error, numInstallations)
	for i := 0; i < numInstallations; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			instID := fmt.Sprintf("inst-%d", i)
			results[i], errs[i] = client.GetInstallationToken(context.Background(), instID)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("installation %d error: %v", i, err)
		}
		wantTok := fmt.Sprintf("ghs_token_inst-%d", i)
		if results[i] != wantTok {
			t.Errorf("installation %d: expected %q, got %q", i, wantTok, results[i])
		}
	}

	// Each installation should have triggered exactly one API call.
	if n := atomic.LoadInt64(&apiCallCount); n != numInstallations {
		t.Errorf("expected %d API calls (one per installation), got %d", numInstallations, n)
	}
}

// ---------------------------------------------------------------------------
// 5. Refresh failure — error surfaces clearly, stale token NOT reused
// ---------------------------------------------------------------------------

// TestGetInstallationToken_RefreshFailureSurfacesError asserts that when the
// GitHub API returns a non-2xx response the caller receives a meaningful
// error, and no stale token is silently returned.
func TestGetInstallationToken_RefreshFailureSurfacesError(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "internal server error",
		})
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	// Seed an expired stale token to confirm it is not returned on error.
	client.tokenCacheMu.Lock()
	client.tokenCache["inst-fail"] = cachedToken{
		token:     "ghs_stale_token",
		expiresAt: now.Add(-time.Hour), // hard-expired
	}
	client.tokenCacheMu.Unlock()

	_, err := client.GetInstallationToken(context.Background(), "inst-fail")
	if err == nil {
		t.Fatal("expected an error when the API call fails, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention HTTP status 500, got: %v", err)
	}
}

// TestGetInstallationToken_RefreshFailureNoCacheEntry asserts that a failure
// on a cold cache (no entry at all) returns an error rather than an empty
// token string.
func TestGetInstallationToken_RefreshFailureNoCacheEntry(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{"message": "Bad credentials"})
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	tok, err := client.GetInstallationToken(context.Background(), "inst-cold-fail")
	if err == nil {
		t.Fatalf("expected error, got token %q", tok)
	}
	if tok != "" {
		t.Errorf("expected empty token on failure, got %q", tok)
	}
}

// TestGetInstallationToken_NotFoundError asserts that a 404 response returns
// the typed ErrInstallationNotFound sentinel.
func TestGetInstallationToken_NotFoundError(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"message": "Not Found"})
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	_, err := client.GetInstallationToken(context.Background(), "inst-gone")
	if err == nil {
		t.Fatal("expected ErrInstallationNotFound, got nil")
	}
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Errorf("expected errors.Is(err, ErrInstallationNotFound), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. Cache isolation between installations
// ---------------------------------------------------------------------------

// TestGetInstallationToken_CacheIsolation asserts that two installations each
// maintain independent cache entries and independent token values.
func TestGetInstallationToken_CacheIsolation(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		instID := parts[len(parts)-2]
		tokenResponse(w, "ghs_token_for_"+instID, newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	tokA, err := client.GetInstallationToken(context.Background(), "inst-A")
	if err != nil {
		t.Fatalf("inst-A error: %v", err)
	}
	tokB, err := client.GetInstallationToken(context.Background(), "inst-B")
	if err != nil {
		t.Fatalf("inst-B error: %v", err)
	}

	if tokA == tokB {
		t.Errorf("expected distinct tokens for distinct installations, both returned %q", tokA)
	}
	if tokA != "ghs_token_for_inst-A" {
		t.Errorf("inst-A: expected %q, got %q", "ghs_token_for_inst-A", tokA)
	}
	if tokB != "ghs_token_for_inst-B" {
		t.Errorf("inst-B: expected %q, got %q", "ghs_token_for_inst-B", tokB)
	}

	// Second call for inst-A must come from cache (no extra HTTP call).
	callCount := 0
	// We can't intercept only the second call via the existing server, so we
	// assert indirectly: the cached value must match what was returned earlier.
	client.tokenCacheMu.Lock()
	ctA := client.tokenCache["inst-A"]
	ctB := client.tokenCache["inst-B"]
	client.tokenCacheMu.Unlock()

	_ = callCount
	if ctA.token != tokA {
		t.Errorf("cache for inst-A mismatch: %q vs %q", ctA.token, tokA)
	}
	if ctB.token != tokB {
		t.Errorf("cache for inst-B mismatch: %q vs %q", ctB.token, tokB)
	}
}

// ---------------------------------------------------------------------------
// 7. cachedToken.isValid unit tests
// ---------------------------------------------------------------------------

// TestCachedToken_IsValid directly exercises the isValid boundary logic to
// ensure the refresh-ahead window is asserted at the unit level.
func TestCachedToken_IsValid(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{
			name:      "empty_token",
			expiresAt: now.Add(time.Hour),
			want:      false, // empty token string → invalid regardless of expiry
		},
		{
			name:      "far_future",
			expiresAt: now.Add(time.Hour),
			want:      true,
		},
		{
			name:      "just_outside_refresh_window",
			expiresAt: now.Add(tokenRefreshAheadWindow + time.Second),
			want:      true,
		},
		{
			name:      "at_refresh_window_boundary",
			expiresAt: now.Add(tokenRefreshAheadWindow),
			want:      false,
		},
		{
			name:      "inside_refresh_window",
			expiresAt: now.Add(tokenRefreshAheadWindow - time.Second),
			want:      false,
		},
		{
			name:      "already_expired",
			expiresAt: now.Add(-time.Minute),
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := "ghs_test"
			if tc.name == "empty_token" {
				token = ""
			}
			ct := cachedToken{token: token, expiresAt: tc.expiresAt}
			got := ct.isValid(now)
			if got != tc.want {
				t.Errorf("isValid(%v): expected %v, got %v (expiresAt=%v, window=%v)",
					tc.name, tc.want, got, tc.expiresAt, tokenRefreshAheadWindow)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 8. Second call uses cache (no redundant API calls after warm-up)
// ---------------------------------------------------------------------------

// TestGetInstallationToken_SecondCallUsesCache verifies that the second call
// for the same installation hits the cache and makes no additional API calls.
func TestGetInstallationToken_SecondCallUsesCache(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		tokenResponse(w, "ghs_only_once", newTokenExpiresAt(t, now, time.Hour))
	}))
	defer server.Close()

	client := buildTokenClient(t, server, now)

	// First call populates the cache.
	tok1, err := client.GetInstallationToken(context.Background(), "inst-reuse")
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}

	// Second call should serve from cache.
	tok2, err := client.GetInstallationToken(context.Background(), "inst-reuse")
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}

	if tok1 != tok2 {
		t.Errorf("tokens should be identical: %q vs %q", tok1, tok2)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 API call, got %d", callCount)
	}
}
