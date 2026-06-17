package auth

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

func TestCreateNonceRequiresPool(t *testing.T) {
	if _, err := CreateNonce(context.Background(), nil, WalletTypeEVM, "0xabc", time.Minute); err == nil {
		t.Fatal("CreateNonce with nil pool returned nil error")
	}
}

func TestConsumeNonceAndUpsertUserRequiresPool(t *testing.T) {
	if _, err := ConsumeNonceAndUpsertUser(context.Background(), nil, WalletTypeEVM, "0xabc", "nonce", ""); err == nil {
		t.Fatal("ConsumeNonceAndUpsertUser with nil pool returned nil error")
	}
}

func TestRandomNonceShape(t *testing.T) {
	nonce := randomNonce(32)
	if nonce == "" {
		t.Fatal("randomNonce returned empty string")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil {
		t.Fatalf("randomNonce returned non-base64url value: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded nonce length = %d, want 32", len(decoded))
	}
}

func TestNullIfEmpty(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Fatalf("nullIfEmpty(empty) = %#v, want nil", got)
	}
	if got := nullIfEmpty("value"); got != "value" {
		t.Fatalf("nullIfEmpty(value) = %#v, want value", got)
	}
}
