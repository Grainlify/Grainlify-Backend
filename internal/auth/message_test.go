package auth

import "testing"

func TestLoginMessageFormats(t *testing.T) {
	if got, want := LoginMessage("abc"), "Grainlify login. Nonce: abc"; got != want {
		t.Fatalf("LoginMessage = %q, want %q", got, want)
	}
	if got, want := LegacyLoginMessage("abc"), "Grainlify login\nNonce: abc"; got != want {
		t.Fatalf("LegacyLoginMessage = %q, want %q", got, want)
	}
}
