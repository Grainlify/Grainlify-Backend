package auth

import "fmt"

// LoginMessage builds the standard message that clients must sign to authenticate.
// Expected template: "Grainlify login. Nonce: {{nonce}}"
// Keep this stable; clients must sign this exact string.
func LoginMessage(nonce string) string {
	// Keep this stable; clients must sign this exact string.
	return fmt.Sprintf("Grainlify login. Nonce: %s", nonce)
}

// LegacyLoginMessage is kept temporarily for compatibility with early clients/tests.
func LegacyLoginMessage(nonce string) string {
	return fmt.Sprintf("Grainlify login\nNonce: %s", nonce)
}
