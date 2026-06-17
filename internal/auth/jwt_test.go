package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestIssueAndParseJWTRoundTrip(t *testing.T) {
	secret := "test-secret"
	userID := uuid.New()

	token, err := IssueJWT(secret, userID, "admin", WalletTypeEVM, "0xabc", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}

	claims, err := ParseJWT(secret, token)
	if err != nil {
		t.Fatalf("ParseJWT returned error: %v", err)
	}

	if claims.Subject != userID.String() {
		t.Fatalf("subject = %q, want %q", claims.Subject, userID.String())
	}
	if claims.Role != "admin" {
		t.Fatalf("role = %q, want admin", claims.Role)
	}
	if claims.WalletType != string(WalletTypeEVM) {
		t.Fatalf("wallet type = %q, want %q", claims.WalletType, WalletTypeEVM)
	}
	if claims.Address != "0xabc" {
		t.Fatalf("address = %q, want 0xabc", claims.Address)
	}
}

func TestIssueJWTDefaultTTL(t *testing.T) {
	before := time.Now()
	token, err := IssueJWT("test-secret", uuid.New(), "user", WalletTypeEVM, "0xabc", 0)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}

	claims, err := ParseJWT("test-secret", token)
	if err != nil {
		t.Fatalf("ParseJWT returned error: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil")
	}

	minExpiry := before.Add(14 * time.Minute)
	maxExpiry := time.Now().Add(16 * time.Minute)
	if claims.ExpiresAt.Time.Before(minExpiry) || claims.ExpiresAt.Time.After(maxExpiry) {
		t.Fatalf("default expiry = %s, want between %s and %s", claims.ExpiresAt.Time, minExpiry, maxExpiry)
	}
}

func TestJWTRejectsInvalidSecretsAndTokens(t *testing.T) {
	if _, err := IssueJWT("", uuid.New(), "user", WalletTypeEVM, "0xabc", time.Minute); err == nil {
		t.Fatal("IssueJWT with empty secret returned nil error")
	}
	if _, err := ParseJWT("", "token"); err == nil {
		t.Fatal("ParseJWT with empty secret returned nil error")
	}

	token, err := IssueJWT("right-secret", uuid.New(), "user", WalletTypeEVM, "0xabc", time.Minute)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}
	if _, err := ParseJWT("wrong-secret", token); err == nil {
		t.Fatal("ParseJWT with wrong secret returned nil error")
	}
}

func TestParseJWTRejectsUnexpectedSigningMethods(t *testing.T) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "user",
	}

	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to create none token: %v", err)
	}
	if _, err := ParseJWT("test-secret", noneToken); err == nil {
		t.Fatal("ParseJWT accepted alg=none token")
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	rsToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("failed to create RS256 token: %v", err)
	}
	if _, err := ParseJWT("test-secret", rsToken); err == nil {
		t.Fatal("ParseJWT accepted RS256 token")
	}
}
