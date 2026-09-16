package handlers

import (
	"context"
	"crypto/ecdsa"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// EVM payout address registration. Grainlify-Backend#548.

// evmChain seeds an enabled EVM chain and removes it afterwards.
//
// Its own chain rather than 'base': this package must not depend on which
// production chains happen to be seeded, and a test that silently starts
// passing because somebody added a row elsewhere is not pinning anything.
func evmChain(t *testing.T, d *db.DB, chainID string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO chain_configs (chain_id, family, enabled, asset, min_confirmations, network, evm_chain_id)
		VALUES ($1,'evm',$2, jsonb_build_object('symbol','USDC','decimals',6), 1, 'testnet', $3)
		ON CONFLICT (chain_id) DO UPDATE SET family='evm', enabled=$2`,
		chainID, enabled, testEVMChainID()); err != nil {
		t.Fatalf("seed chain %s: %v", chainID, err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM chain_configs WHERE chain_id=$1`, chainID)
	})
}

// testEVMChainID is a numeric chain id unique per call, far above any real
// chain. Unique rather than fixed: the column is UNIQUE and this database is
// never truncated, so a fixed value left behind by one interrupted run would
// break every run after it.
func testEVMChainID() int64 { return 900_000_000_000 + int64(uuid.New().ID()) }

// registerEVM drives the real challenge/sign/register flow with a real secp256k1 key.
func registerEVM(t *testing.T, d *db.DB, uid uuid.UUID, priv *ecdsa.PrivateKey, chainID, address string) (int, map[string]any) {
	t.Helper()
	app := addrApp(uid, d)

	code, ch := postJSON(t, app, "/me/payout-address/challenge",
		map[string]any{"chain_id": chainID, "address": address})
	if code != 200 {
		return code, ch
	}
	msg, _ := ch["message"].(string)
	nonce, _ := ch["nonce"].(string)

	// The same hash verifyEVM recovers against: an EIP-191 personal-sign text
	// hash, not the raw message.
	sig, err := crypto.Sign(accounts.TextHash([]byte(msg)), priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return postJSON(t, app, "/me/payout-address", map[string]any{
		"chain_id":  chainID,
		"address":   address,
		"signature": hexutil.Encode(sig),
		"nonce":     nonce,
	})
}

func TestRegisterEVM_HappyPathStoresTheChecksummedAddress(t *testing.T) {
	d := dbtest.DB(t)
	evmChain(t, d, "evm-happy", true)

	priv, _ := crypto.GenerateKey()
	want := crypto.PubkeyToAddress(priv.PublicKey).Hex()

	uid := newUser(t, d)
	code, body := registerEVM(t, d, uid, priv, "evm-happy", want)
	if code != 201 {
		t.Fatalf("register: %d %v", code, body)
	}

	var got, family string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT address, chain_family FROM contributor_addresses WHERE user_id=$1`, uid).
		Scan(&got, &family); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("stored %q, want the EIP-55 form %q", got, want)
	}
	if family != "evm" {
		t.Errorf("chain_family = %q, want evm", family)
	}
	if len(got) != 42 {
		t.Fatalf("stored address is %d characters, want 42 - anything longer means it "+
			"was padded, which is the #548 defect", len(got))
	}
}

// The test #548 asks for by name: a 40-hex input under an EVM chain_id must not
// become a 64-hex Aptos address.
//
// Asserted against the DATABASE rather than the response, because the padding
// would be invisible in a 201 - the value is well-formed, it stores, and it is
// simply somebody else's address.
func TestRegisterEVM_A40HexAddressIsNeverPaddedIntoAnAptosAddress(t *testing.T) {
	d := dbtest.DB(t)
	evmChain(t, d, "evm-nopad", true)

	priv, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(priv.PublicKey).Hex()

	uid := newUser(t, d)
	if code, body := registerEVM(t, d, uid, priv, "evm-nopad", addr); code != 201 {
		t.Fatalf("register: %d %v", code, body)
	}

	var stored string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT address FROM contributor_addresses WHERE user_id=$1`, uid).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) == 66 {
		t.Fatalf("the EVM address was padded to a 64-hex Aptos address (%s) - "+
			"this is exactly #548, and the money would go to an address nobody controls", stored)
	}
	if stored != addr {
		t.Errorf("stored %q, want %q unchanged", stored, addr)
	}
}

// An Aptos address must not register under an EVM chain. The companion gap in
// #548: chain_id was free text, so this was accepted.
func TestRegisterEVM_RefusesAnAptosAddressUnderAnEVMChain(t *testing.T) {
	d := dbtest.DB(t)
	evmChain(t, d, "evm-aptosaddr", true)

	const aptos = "0x000000000000000000000000106175f175b940cca1816d75eb19937a88be7720"
	code, body := postJSON(t, addrApp(newUser(t, d), d), "/me/payout-address/challenge",
		map[string]any{"chain_id": "evm-aptosaddr", "address": aptos})
	if code != 400 {
		t.Fatalf("status %d, want 400 for a 64-hex address on an EVM chain (%v)", code, body)
	}
}

// chain_id stops being any non-empty string.
func TestRegisterEVM_RefusesUnconfiguredAndDisabledChains(t *testing.T) {
	d := dbtest.DB(t)
	evmChain(t, d, "evm-disabled", false)

	priv, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(priv.PublicKey).Hex()
	uid := newUser(t, d)

	for _, tc := range []struct{ chain, want string }{
		{"no-such-chain-at-all", "chain_not_configured"},
		{"evm-disabled", "chain_not_enabled"},
	} {
		code, body := postJSON(t, addrApp(uid, d), "/me/payout-address/challenge",
			map[string]any{"chain_id": tc.chain, "address": addr})
		if code != 400 {
			t.Errorf("%s: status %d, want 400 (%v)", tc.chain, code, body)
			continue
		}
		if body["error"] != tc.want {
			t.Errorf("%s: error = %v, want %q - one name per cause", tc.chain, body["error"], tc.want)
		}
	}
}

// A signature from a different key must not register the claimed address.
func TestRegisterEVM_RefusesASignatureFromAnotherKey(t *testing.T) {
	d := dbtest.DB(t)
	evmChain(t, d, "evm-wrongkey", true)

	priv, _ := crypto.GenerateKey()
	other, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(priv.PublicKey).Hex()

	uid := newUser(t, d)
	// Sign the challenge for `addr` with a key that does not own it.
	code, body := registerEVM(t, d, uid, other, "evm-wrongkey", addr)
	if code != 400 {
		t.Fatalf("status %d, want 400 - the signature was made by a key that does not "+
			"own %s (%v)", code, addr, body)
	}
	if body["error"] != "signature_invalid" {
		t.Errorf("error = %v, want signature_invalid", body["error"])
	}
}
