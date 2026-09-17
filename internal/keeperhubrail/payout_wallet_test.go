package keeperhubrail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The resolve hint on the admin screen names this address as the sender to
// look for. A wrong one is worse than none, so anything that is not a valid
// EVM address is reported as null, and a valid one comes back EIP-55.
func TestPayoutWalletView_ValidatesAndNormalises(t *testing.T) {
	const lower = "0xe6e5e247ce27a43f724675dd679dc7a4a1896ca6"
	const checksummed = "0xE6e5e247ce27A43F724675DD679DC7a4a1896CA6"

	for name, tc := range map[string]struct {
		raw  string
		want string // "" means null
	}{
		"unset":                {"", ""},
		"blank":                {"   ", ""},
		"lowercase":            {lower, checksummed},
		"checksummed":          {checksummed, checksummed},
		"too short":            {"0xe6e5e247ce27a43f", ""},
		"bad checksum":         {"0xE6E5e247ce27A43F724675DD679DC7a4a1896CA6", ""},
		"zero address":         {"0x0000000000000000000000000000000000000000", ""},
		"aptos-length address": {"0x" + strings.Repeat("ab", 32), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := payoutWalletView(tc.raw)
			if got.Note == "" {
				t.Fatal("the note saying this is current configuration must always be present")
			}
			switch {
			case tc.want == "" && got.Address != nil:
				t.Fatalf("got %q, want null", *got.Address)
			case tc.want != "" && (got.Address == nil || *got.Address != tc.want):
				t.Fatalf("got %v, want %q", got.Address, tc.want)
			}
		})
	}
}

func TestRunView_CarriesTheConfiguredPayoutWallet(t *testing.T) {
	f := fixture(t)
	s, _, _ := partialFailureRun(t, f)

	s.PayoutWallet = "0xe6e5e247ce27a43f724675dd679dc7a4a1896ca6"
	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.PayoutWallet.Address == nil || *v.PayoutWallet.Address != "0xE6e5e247ce27A43F724675DD679DC7a4a1896CA6" {
		t.Fatalf("payout wallet = %v", v.PayoutWallet.Address)
	}

	// Unset is an explicit null in the JSON, not a missing key and not "".
	s.PayoutWallet = ""
	v, err = s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var pw map[string]any
	if err := json.Unmarshal(raw["payout_wallet"], &pw); err != nil {
		t.Fatalf("payout_wallet missing or not an object: %v", err)
	}
	if addr, present := pw["address"]; !present || addr != nil {
		t.Fatalf("unset address should be null, got %#v (present=%v)", addr, present)
	}
}
