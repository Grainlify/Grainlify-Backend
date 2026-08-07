package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// jwtTestSecret is the HMAC secret used to sign/verify tokens throughout
// this file's tests.
const jwtTestSecret = "jwt-test-secret-do-not-use-in-prod"

func TestIssueJWT_ParseJWT_RoundTrip(t *testing.T) {
	userID := uuid.New()
	token, err := IssueJWT(jwtTestSecret, userID, "maintainer", WalletTypeEVM, "0xdeadbeef", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	if token == "" {
		t.Fatal("IssueJWT returned an empty token")
	}

	claims, err := ParseJWT(jwtTestSecret, token)
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if claims.Subject != userID.String() {
		t.Errorf("Subject = %v, want %v", claims.Subject, userID.String())
	}
	if claims.Role != "maintainer" {
		t.Errorf("Role = %v, want maintainer", claims.Role)
	}
	if claims.WalletType != string(WalletTypeEVM) {
		t.Errorf("WalletType = %v, want %v", claims.WalletType, WalletTypeEVM)
	}
	if claims.Address != "0xdeadbeef" {
		t.Errorf("Address = %v, want 0xdeadbeef", claims.Address)
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatal("expected ExpiresAt and IssuedAt to be set")
	}
	if !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) {
		t.Errorf("ExpiresAt (%v) should be after IssuedAt (%v)", claims.ExpiresAt.Time, claims.IssuedAt.Time)
	}
	gotTTL := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time)
	if gotTTL < 59*time.Minute || gotTTL > 61*time.Minute {
		t.Errorf("ttl = %v, want ~1h", gotTTL)
	}
}

func TestIssueJWT_DefaultTTLWhenNonPositive(t *testing.T) {
	userID := uuid.New()
	for _, ttl := range []time.Duration{0, -time.Minute} {
		token, err := IssueJWT(jwtTestSecret, userID, "contributor", WalletTypeEVM, "0xabc", ttl)
		if err != nil {
			t.Fatalf("IssueJWT(ttl=%v): %v", ttl, err)
		}
		claims, err := ParseJWT(jwtTestSecret, token)
		if err != nil {
			t.Fatalf("ParseJWT: %v", err)
		}
		got := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time)
		if got < 14*time.Minute || got > 16*time.Minute {
			t.Errorf("ttl=%v: default expiry window = %v, want ~15m", ttl, got)
		}
	}
}

func TestIssueJWT_EmptySecret(t *testing.T) {
	_, err := IssueJWT("", uuid.New(), "contributor", WalletTypeEVM, "0xabc", time.Hour)
	if err == nil {
		t.Fatal("expected error for empty secret, got nil")
	}
}

func TestParseJWT_EmptySecret(t *testing.T) {
	_, err := ParseJWT("", "irrelevant-token")
	if err == nil {
		t.Fatal("expected error for empty secret, got nil")
	}
}

func TestParseJWT_MalformedToken(t *testing.T) {
	_, err := ParseJWT(jwtTestSecret, "this-is-not-a-jwt")
	if err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestParseJWT_WrongSecret(t *testing.T) {
	token, err := IssueJWT(jwtTestSecret, uuid.New(), "contributor", WalletTypeEVM, "0xabc", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	if _, err := ParseJWT("a-completely-different-secret", token); err == nil {
		t.Fatal("expected error when parsing with the wrong secret, got nil")
	}
}

func TestParseJWT_ExpiredToken(t *testing.T) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Hour)),
		},
		Role: "contributor",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(jwtTestSecret))
	if err != nil {
		t.Fatalf("sign expired claims: %v", err)
	}

	if _, err := ParseJWT(jwtTestSecret, signed); err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestParseJWT_WrongSigningMethod(t *testing.T) {
	// ParseJWT explicitly rejects anything that isn't HS256.
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "contributor",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	signed, err := tok.SignedString([]byte(jwtTestSecret))
	if err != nil {
		t.Fatalf("sign claims with HS384: %v", err)
	}

	if _, err := ParseJWT(jwtTestSecret, signed); err == nil {
		t.Fatal("expected error for non-HS256 token, got nil")
	}
}
