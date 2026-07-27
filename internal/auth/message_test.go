package auth

import (
	"crypto/ecdsa"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginMessageFormat(t *testing.T) {
	nonce := "1234567890"
	msg := LoginMessage(nonce)

	// Structural invariants
	assert.Contains(t, msg, "Grainlify login.", "Message must contain the correct domain/statement")
	assert.Contains(t, msg, "Nonce: "+nonce, "Message must correctly place the nonce")
	assert.True(t, strings.HasSuffix(msg, nonce), "Message should end with the nonce")
	
	// Exact expected format
	expected := "Grainlify login. Nonce: 1234567890"
	assert.Equal(t, expected, msg, "Message format must exactly match the locked-in wire format")
}

func TestLegacyLoginMessageFormat(t *testing.T) {
	nonce := "1234567890"
	msg := LegacyLoginMessage(nonce)
	expected := "Grainlify login\nNonce: 1234567890"
	assert.Equal(t, expected, msg, "Legacy message format must exactly match")
}

func signMessageEVM(t *testing.T, privateKey *ecdsa.PrivateKey, message string) string {
	hash := accounts.TextHash([]byte(message))
	signatureBytes, err := crypto.Sign(hash, privateKey)
	require.NoError(t, err)

	return hexutil.Encode(signatureBytes)
}

func TestLoginMessage_VerifyRejectsMalformed(t *testing.T) {
	// Generate a test EVM key
	privateKey, err := crypto.GenerateKey()
	require.NoError(t, err)

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	require.True(t, ok)

	address := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
	expectedNonce := "secure-nonce-123"

	// The server constructs the expected message using the correct format
	expectedMessage := LoginMessage(expectedNonce)

	t.Run("Happy path - valid signature", func(t *testing.T) {
		sig := signMessageEVM(t, privateKey, expectedMessage)
		err := VerifySignature(WalletTypeEVM, address, expectedMessage, sig, "")
		assert.NoError(t, err, "Valid signature should be accepted")
	})

	t.Run("Rejects missing nonce", func(t *testing.T) {
		// Client signs a malformed message (missing nonce)
		malformedMessage := "Grainlify login. Nonce: "
		sig := signMessageEVM(t, privateKey, malformedMessage)
		err := VerifySignature(WalletTypeEVM, address, expectedMessage, sig, "")
		assert.ErrorContains(t, err, "signature does not match address", "Should reject signature for message missing nonce")
	})

	t.Run("Rejects mismatched domain", func(t *testing.T) {
		// Client signs a message with a wrong domain
		wrongDomainMessage := "OtherApp login. Nonce: " + expectedNonce
		sig := signMessageEVM(t, privateKey, wrongDomainMessage)
		err := VerifySignature(WalletTypeEVM, address, expectedMessage, sig, "")
		assert.ErrorContains(t, err, "signature does not match address", "Should reject signature for message with wrong domain")
	})

	t.Run("Rejects extra unexpected fields appended", func(t *testing.T) {
		// Client signs a message with extra unexpected fields appended
		extraFieldsMessage := expectedMessage + "\nExtra: data"
		sig := signMessageEVM(t, privateKey, extraFieldsMessage)
		err := VerifySignature(WalletTypeEVM, address, expectedMessage, sig, "")
		assert.ErrorContains(t, err, "signature does not match address", "Should reject signature for message with extra fields")
	})
}
