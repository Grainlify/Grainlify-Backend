package auth

import (
	"strings"
	"testing"
)

func TestLoginMessage(t *testing.T) {
	cases := []struct {
		name  string
		nonce string
		want  string
	}{
		{"typical nonce", "abc123", "Patchwork login. Nonce: abc123"},
		{"empty nonce", "", "Patchwork login. Nonce: "},
		{"nonce with special characters", "a/b+c==", "Patchwork login. Nonce: a/b+c=="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LoginMessage(tc.nonce)
			if got != tc.want {
				t.Errorf("LoginMessage(%q) = %q, want %q", tc.nonce, got, tc.want)
			}
		})
	}
}

func TestLoginMessage_ContainsNonce(t *testing.T) {
	nonce := "unique-nonce-value-12345"
	got := LoginMessage(nonce)
	if !strings.Contains(got, nonce) {
		t.Errorf("LoginMessage(%q) = %q, want it to contain the nonce", nonce, got)
	}
}

func TestLoginMessage_Deterministic(t *testing.T) {
	nonce := "same-nonce"
	a := LoginMessage(nonce)
	b := LoginMessage(nonce)
	if a != b {
		t.Errorf("LoginMessage is not deterministic for the same input: %q != %q", a, b)
	}
}

func TestLoginMessage_DifferentNoncesProduceDifferentMessages(t *testing.T) {
	a := LoginMessage("nonce-one")
	b := LoginMessage("nonce-two")
	if a == b {
		t.Error("expected different nonces to produce different messages")
	}
}

func TestLegacyLoginMessage(t *testing.T) {
	cases := []struct {
		name  string
		nonce string
		want  string
	}{
		{"typical nonce", "abc123", "Patchwork login\nNonce: abc123"},
		{"empty nonce", "", "Patchwork login\nNonce: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LegacyLoginMessage(tc.nonce)
			if got != tc.want {
				t.Errorf("LegacyLoginMessage(%q) = %q, want %q", tc.nonce, got, tc.want)
			}
		})
	}
}

func TestLegacyLoginMessage_ContainsNonce(t *testing.T) {
	nonce := "unique-legacy-nonce-6789"
	got := LegacyLoginMessage(nonce)
	if !strings.Contains(got, nonce) {
		t.Errorf("LegacyLoginMessage(%q) = %q, want it to contain the nonce", nonce, got)
	}
}

func TestLegacyLoginMessage_Deterministic(t *testing.T) {
	nonce := "same-legacy-nonce"
	a := LegacyLoginMessage(nonce)
	b := LegacyLoginMessage(nonce)
	if a != b {
		t.Errorf("LegacyLoginMessage is not deterministic for the same input: %q != %q", a, b)
	}
}

func TestLegacyLoginMessage_DiffersFromLoginMessage(t *testing.T) {
	nonce := "same-nonce-both-formats"
	if LoginMessage(nonce) == LegacyLoginMessage(nonce) {
		t.Error("expected LoginMessage and LegacyLoginMessage to differ (different separator between prefix and nonce)")
	}
}
