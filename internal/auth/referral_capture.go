package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Referral capture tokens make the published "a click counts for 30 days"
// rule enforceable.
//
// The window used to live only in the browser: the frontend stored a capture
// timestamp in localStorage and declined to send an expired code. But the
// login-start endpoint accepted any `ref` handed to it, so the rule could be
// bypassed by calling it directly with an eight-month-old code - a published
// rule the backend did not enforce, which is the same class of problem as a
// consent screen that contradicts the sign-in copy.
//
// The capture endpoint now signs {code, capturedAt} and the browser stores
// the signed token instead of the bare code. The client cannot backdate it,
// because it cannot forge the signature, and it cannot extend it, because the
// expiry is the JWT's own `exp`.
//
// Note what this deliberately does NOT prevent: anyone can call the capture
// endpoint right now and get a fresh token. That is not a bypass - it is a
// click, which is exactly what the rule counts. What it stops is a *stale*
// click being presented as a fresh one.

// ReferralCaptureClaims is a referral code plus the standard expiry claims.
// Deliberately not the user Claims type: this token authenticates nobody, and
// sharing a shape with the session token invites one being accepted where the
// other is expected.
type ReferralCaptureClaims struct {
	jwt.RegisteredClaims
	ReferralCode string `json:"ref"`
}

// IssueReferralCapture signs a code with a capture time and a TTL.
func IssueReferralCapture(secret, code string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("JWT_SECRET is required")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("referral code is required")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("referral capture ttl must be positive")
	}

	now := time.Now()
	claims := ReferralCaptureClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			// Subject names what this token is for, so a session token can
			// never be mistaken for one of these by a future reader.
			Subject:   "referral_capture",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		ReferralCode: code,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// ParseReferralCapture verifies a capture token and returns its code.
//
// Fails closed in every direction: a bad signature, an expired token, a
// different algorithm, or a token issued for something else all yield an
// error, and the caller treats that as "no referral" rather than falling back
// to an unverified value.
func ParseReferralCapture(secret, tokenString string) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("JWT_SECRET is required")
	}
	var claims ReferralCaptureClaims
	_, err := jwt.ParseWithClaims(tokenString, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return "", err
	}
	if claims.Subject != "referral_capture" {
		return "", fmt.Errorf("not a referral capture token")
	}
	if strings.TrimSpace(claims.ReferralCode) == "" {
		return "", fmt.Errorf("referral capture token carries no code")
	}
	return claims.ReferralCode, nil
}
