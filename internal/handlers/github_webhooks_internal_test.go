package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// ghWebhookInternalSign computes a GitHub-style X-Hub-Signature-256 header
// value ("sha256=" + hex HMAC) independently of verifyGitHubSignature's own
// hex encoding, so the test exercises verifyGitHubSignature against a
// signature it did not produce itself.
func ghWebhookInternalSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubSignature(t *testing.T) {
	const secret = "gh-webhook-internal-test-secret"
	body := []byte(`{"action":"opened","repository":{"full_name":"octocat/Hello-World"}}`)
	validHeader := ghWebhookInternalSign(secret, body)

	t.Run("valid signature verifies", func(t *testing.T) {
		if !verifyGitHubSignature(secret, body, validHeader) {
			t.Error("verifyGitHubSignature() = false, want true for a correctly signed body")
		}
	})

	t.Run("wrong secret fails", func(t *testing.T) {
		if verifyGitHubSignature("a-completely-different-secret", body, validHeader) {
			t.Error("verifyGitHubSignature() = true, want false when secret does not match")
		}
	})

	t.Run("tampered body fails", func(t *testing.T) {
		tampered := append(append([]byte(nil), body...), '!')
		if verifyGitHubSignature(secret, tampered, validHeader) {
			t.Error("verifyGitHubSignature() = true, want false when body was modified after signing")
		}
	})

	t.Run("truncated body fails", func(t *testing.T) {
		truncated := body[:len(body)-1]
		if verifyGitHubSignature(secret, truncated, validHeader) {
			t.Error("verifyGitHubSignature() = true, want false when body is truncated")
		}
	})

	t.Run("missing sha256 prefix fails", func(t *testing.T) {
		rawHex := strings.TrimPrefix(validHeader, "sha256=")
		if verifyGitHubSignature(secret, body, rawHex) {
			t.Error("verifyGitHubSignature() = true, want false when header lacks the sha256= prefix")
		}
	})

	t.Run("non-hex garbage after prefix fails", func(t *testing.T) {
		if verifyGitHubSignature(secret, body, "sha256=not-hex-garbage!!") {
			t.Error("verifyGitHubSignature() = true, want false for non-hex garbage")
		}
	})

	t.Run("empty header fails", func(t *testing.T) {
		if verifyGitHubSignature(secret, body, "") {
			t.Error("verifyGitHubSignature() = true, want false for an empty header")
		}
	})

	t.Run("wrong prefix case (sha1=) fails", func(t *testing.T) {
		rawHex := strings.TrimPrefix(validHeader, "sha256=")
		if verifyGitHubSignature(secret, body, "sha1="+rawHex) {
			t.Error("verifyGitHubSignature() = true, want false for a sha1= prefixed header")
		}
	})

	t.Run("empty secret and empty body still requires a valid signature", func(t *testing.T) {
		if verifyGitHubSignature("", nil, "") {
			t.Error("verifyGitHubSignature() = true, want false for an empty header even with empty secret/body")
		}
		emptyHeader := ghWebhookInternalSign("", nil)
		if !verifyGitHubSignature("", nil, emptyHeader) {
			t.Error("verifyGitHubSignature() = false, want true for a correctly signed empty body with empty secret")
		}
	})
}

func TestHexEncodeLower(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"nil input", nil, ""},
		{"empty input", []byte{}, ""},
		{"single zero byte", []byte{0x00}, "00"},
		{"single max byte", []byte{0xff}, "ff"},
		{"known bytes", []byte{0x00, 0xff, 0x10, 0xab, 0x5c}, "00ff10ab5c"},
		{"ascii text", []byte("hi"), "6869"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hexEncodeLower(tt.in)
			if got != tt.want {
				t.Errorf("hexEncodeLower(%v) = %q, want %q", tt.in, got, tt.want)
			}
			// Cross-check against the standard library as an independent oracle.
			if want := hex.EncodeToString(tt.in); got != want {
				t.Errorf("hexEncodeLower(%v) = %q, want stdlib hex.EncodeToString() = %q", tt.in, got, want)
			}
			// hexEncodeLower must always be lowercase.
			if got != strings.ToLower(got) {
				t.Errorf("hexEncodeLower(%v) = %q, contains uppercase characters", tt.in, got)
			}
		})
	}
}
