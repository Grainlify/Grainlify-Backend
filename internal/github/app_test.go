package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	})

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

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}
