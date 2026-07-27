package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func withJWTNow(t *testing.T, now time.Time) {
	t.Helper()
	original := jwtNow
	jwtNow = func() time.Time { return now }
	t.Cleanup(func() { jwtNow = original })
}

func TestIssueAndParseJWTClaims(t *testing.T) {
	issuedAt := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withJWTNow(t, issuedAt)

	secret := "unit-test-jwt-secret"
	userID := uuid.MustParse("d1dd49d6-1f8b-4d33-a8ac-f0a507c6700f")
	ttl := 30 * time.Minute

	token, err := IssueJWT(secret, userID, "admin", WalletTypeEVM, "0xabc", ttl)
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
	if claims.IssuedAt == nil || !claims.IssuedAt.Time.Equal(issuedAt) {
		t.Fatalf("issued_at = %v, want %s", claims.IssuedAt, issuedAt)
	}
	if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(issuedAt.Add(ttl)) {
		t.Fatalf("expires_at = %v, want %s", claims.ExpiresAt, issuedAt.Add(ttl))
	}
}

func TestParseJWTExpiryBoundary(t *testing.T) {
	issuedAt := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	secret := "unit-test-jwt-secret"
	userID := uuid.New()
	withJWTNow(t, issuedAt)
	token, err := IssueJWT(secret, userID, "user", WalletTypeEVM, "0xabc", time.Minute)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}

	tests := []struct {
		name      string
		now       time.Time
		wantError error
	}{
		{
			name: "valid one second before expiration",
			now:  issuedAt.Add(time.Minute - time.Second),
		},
		{
			name:      "expired exactly at expiration boundary with no clock skew leeway",
			now:       issuedAt.Add(time.Minute),
			wantError: ErrJWTExpired,
		},
		{
			name:      "expired after expiration",
			now:       issuedAt.Add(time.Minute + time.Second),
			wantError: ErrJWTExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withJWTNow(t, tt.now)
			claims, err := ParseJWT(secret, token)
			if tt.wantError != nil {
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("ParseJWT error = %v, want %v", err, tt.wantError)
				}
				if claims != nil {
					t.Fatalf("claims = %#v, want nil", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJWT returned error: %v", err)
			}
			if claims.Subject != userID.String() {
				t.Fatalf("subject = %q, want %q", claims.Subject, userID.String())
			}
		})
	}
}

func TestIssueJWTDefaultTTL(t *testing.T) {
	issuedAt := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	withJWTNow(t, issuedAt)

	token, err := IssueJWT("test-secret", uuid.New(), "user", WalletTypeEVM, "0xabc", 0)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}

	claims, err := ParseJWT("test-secret", token)
	if err != nil {
		t.Fatalf("ParseJWT returned error: %v", err)
	}
	if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(issuedAt.Add(15*time.Minute)) {
		t.Fatalf("default expiry = %v, want %s", claims.ExpiresAt, issuedAt.Add(15*time.Minute))
	}
}

func TestParseJWTRejectsInvalidTokens(t *testing.T) {
	secret := "right-secret"
	withJWTNow(t, time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	token, err := IssueJWT(secret, uuid.New(), "user", WalletTypeEVM, "0xabc", time.Minute)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}
	tamperedToken := token[:strings.LastIndex(token, ".")+1] + "tampered-signature"

	tests := []struct {
		name    string
		secret  string
		token   string
		wantErr error
	}{
		{
			name:    "empty parse secret",
			secret:  "",
			token:   token,
			wantErr: ErrJWTInvalid,
		},
		{
			name:    "wrong secret rejects signature",
			secret:  "wrong-secret",
			token:   token,
			wantErr: ErrJWTInvalid,
		},
		{
			name:    "tampered signature",
			secret:  secret,
			token:   tamperedToken,
			wantErr: ErrJWTInvalid,
		},
		{
			name:    "malformed token",
			secret:  secret,
			token:   "not-a-jwt",
			wantErr: ErrJWTInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseJWT(tt.secret, tt.token)
			if tt.secret == "" {
				if err == nil {
					t.Fatal("ParseJWT returned nil error")
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseJWT error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	if _, err := IssueJWT("", uuid.New(), "user", WalletTypeEVM, "0xabc", time.Minute); err == nil {
		t.Fatal("IssueJWT with empty secret returned nil error")
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
	if _, err := ParseJWT("test-secret", noneToken); !errors.Is(err, ErrJWTInvalid) {
		t.Fatalf("ParseJWT alg=none error = %v, want %v", err, ErrJWTInvalid)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	rsToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("failed to create RS256 token: %v", err)
	}
	if _, err := ParseJWT("test-secret", rsToken); !errors.Is(err, ErrJWTInvalid) {
		t.Fatalf("ParseJWT RS256 error = %v, want %v", err, ErrJWTInvalid)
	}
}
