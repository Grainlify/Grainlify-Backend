package sponsor

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Submission is the transport half: sign as fee payer, then post.
//
// # Why the SDK is not the client here
//
// The node URL, the endpoint refusal and the error taxonomy live in one place -
// this package and internal/chainread - so that an operator reading a failure
// finds one vocabulary rather than two. An SDK client sitting in the request
// path would carry its own retries, its own timeouts and its own error strings,
// and the first confusing production failure would be about which of the two
// spoke.
//
// So the SDK provides BCS ENCODING ONLY (see signingmessage.go) and this posts
// the result over the same HTTP shape everything else uses.
type Submitter struct {
	nodeURL string
	http    *http.Client
	signer  *Signer
}

func NewSubmitter(nodeURL string, signer *Signer) *Submitter {
	return &Submitter{nodeURL: nodeURL, http: &http.Client{Timeout: 30 * time.Second}, signer: signer}
}

// SubmitSponsored signs as fee payer and submits an already sender-signed
// transaction.
//
// # The fee payer signs a DIFFERENT message from the sender
//
// The sender may have signed with fee_payer_address = 0x0, not knowing who would
// pay. The fee payer always signs with its own address in place, because by then
// it is known. Reusing the sender's message here would produce a fee-payer
// signature over the wrong bytes - the same class of failure the sender-side
// form check exists for, one signer along.
func (s *Submitter) SubmitSponsored(ctx context.Context, f TxnFields, senderPubKey, senderSig []byte) (string, error) {
	// The fee payer's own message always names the real payer.
	payerFields := f
	payerFields.FeePayer = s.feePayerAddress(f)
	msg, err := FeePayerSigningMessage(payerFields)
	if err != nil {
		return "", err
	}
	payerSig := s.signer.Sign(msg)

	body := map[string]any{
		"sender":                    f.Sender,
		"sequence_number":           fmt.Sprintf("%d", f.SeqNumber),
		"max_gas_amount":            fmt.Sprintf("%d", f.MaxGas),
		"gas_unit_price":            fmt.Sprintf("%d", f.GasUnitPrice),
		"expiration_timestamp_secs": fmt.Sprintf("%d", f.Expiration),
		"payload": map[string]any{
			"type":           "entry_function_payload",
			"function":       f.Module + "::escrow::" + f.Function,
			"type_arguments": []string{},
			"arguments":      []any{f.Escrow, f.IdentityHash, fmt.Sprintf("%d", f.AmountMinor), f.Proof},
		},
		"signature": map[string]any{
			"type": "fee_payer_signature",
			"sender": map[string]any{
				"type": "ed25519_signature", "public_key": "0x" + hex.EncodeToString(senderPubKey),
				"signature": "0x" + hex.EncodeToString(senderSig),
			},
			"secondary_signer_addresses": []string{},
			"secondary_signers":          []any{},
			"fee_payer_address":          payerFields.FeePayer,
			"fee_payer_signer": map[string]any{
				"type": "ed25519_signature", "public_key": s.signer.PublicKeyHex(),
				"signature": "0x" + hex.EncodeToString(payerSig),
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.nodeURL+"/v1/transactions", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSubmitFailed, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The upstream status and a bounded body, as everywhere else: a rejected
		// submission usually says exactly why, and discarding it turns a precise
		// answer into "the node said no".
		return "", fmt.Errorf("%w: status %d: %s", ErrSubmitFailed, res.StatusCode, string(out))
	}
	var reply struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.Hash == "" {
		return "", fmt.Errorf("%w: no transaction hash in the reply: %s", ErrSubmitFailed, string(out))
	}
	return reply.Hash, nil
}

// feePayerAddress derives the payer's address from its public key.
//
// Read from the key rather than configured, so the address and the key cannot
// disagree - a configured address that drifts from the key produces a signature
// the node attributes to somebody else.
func (s *Submitter) feePayerAddress(f TxnFields) string {
	if f.FeePayer != "" && f.FeePayer != ZeroAddress {
		return f.FeePayer
	}
	return s.signer.AptosAddress()
}
