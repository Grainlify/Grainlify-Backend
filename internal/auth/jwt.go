package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrJWTExpired = errors.New("jwt expired")
	ErrJWTInvalid = errors.New("jwt invalid")
)

var jwtNow = time.Now

type Claims struct {
	jwt.RegisteredClaims
	Role       string `json:"role"`
	WalletType string `json:"wallet_type,omitempty"`
	Address    string `json:"address,omitempty"`
}

func IssueJWT(secret string, userID uuid.UUID, role string, walletType WalletType, address string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("JWT_SECRET is required")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	now := jwtNow()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Role:       role,
		WalletType: string(walletType),
		Address:    address,
	}

	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

func ParseJWT(secret string, tokenString string) (*Claims, error) {
	if secret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	parsed, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unexpected signing method", ErrJWTInvalid)
		}
		return []byte(secret), nil
	}, jwt.WithTimeFunc(jwtNow))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("%w: %w", ErrJWTExpired, err)
		}
		return nil, fmt.Errorf("%w: %w", ErrJWTInvalid, err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrJWTInvalid
	}
	return claims, nil
}
