package payoutaddr

import (
	"errors"
	"strings"
	"testing"
)

// The defect in #548, pinned from the other side.
//
// This is the whole reason ValidateEVM exists, so it is asserted rather than
// described: the Aptos validator ACCEPTS an EVM address and returns a different
// one. If a future change ever makes Validate reject 40-hex input instead, this
// test fails and should be re-read rather than deleted - the padding is what
// must not reach an EVM path, and this records that it is still there.
func TestValidateAptos_PadsAnEVMAddress_WhichIsWhyEVMHasItsOwnValidator(t *testing.T) {
	const evm = "0x106175F175B940CcA1816d75eB19937a88BE7720"

	got, err := Validate(evm)
	if err != nil {
		t.Fatalf("Validate(%s) errored (%v); #548 describes it SUCCEEDING, so this "+
			"test and the EVM dispatch both need re-reading", evm, err)
	}
	if strings.EqualFold(got, evm) {
		t.Fatalf("Validate returned the same address; the padding #548 describes is gone")
	}
	if got != "0x000000000000000000000000106175f175b940cca1816d75eb19937a88be7720" {
		t.Fatalf("Validate(%s) = %s, want the zero-padded 64-hex form", evm, got)
	}
}

// ValidateEVM must never pad, in either direction.
func TestValidateEVM_NeverPads(t *testing.T) {
	for _, in := range []string{
		"0x1",
		"0x106175F175B940CcA1816d75eB19937a88BE772",   // 39
		"0x106175F175B940CcA1816d75eB19937a88BE77200", // 41
		"0x000000000000000000000000106175f175b940cca1816d75eb19937a88be7720", // 64
	} {
		out, err := ValidateEVM(in)
		if err == nil {
			t.Errorf("ValidateEVM(%q) = %q, want an error - anything not exactly 40 hex "+
				"characters must be refused, never repaired", in, out)
		}
	}
}

func TestValidateEVM_AcceptsAndChecksums(t *testing.T) {
	const want = "0x106175F175B940CcA1816d75eB19937a88BE7720"

	// All-lowercase carries no checksum information, so it cannot be verified
	// and must not be refused - it is the form most explorers hand out. The
	// stored value is still constructed into the EIP-55 spelling.
	got, err := ValidateEVM(strings.ToLower(want))
	if err != nil {
		t.Fatalf("lowercase input: %v", err)
	}
	if got != want {
		t.Errorf("lowercase input returned %q, want the EIP-55 form %q", got, want)
	}

	// Correct mixed case round-trips unchanged.
	if got, err := ValidateEVM(want); err != nil || got != want {
		t.Errorf("ValidateEVM(%q) = %q, %v; want it returned unchanged", want, got, err)
	}
}

// The one check that catches a mistyped address.
//
// Every other rule accepts any 40 hex characters, so without EIP-55 a single
// wrong character is a valid address belonging to nobody and the money is gone.
func TestValidateEVM_RejectsABadChecksum(t *testing.T) {
	// Correct address with its final digit changed, case left mixed - so the
	// input still claims to carry a checksum.
	const typo = "0x106175F175B940CcA1816d75eB19937a88BE7721"

	out, err := ValidateEVM(typo)
	if !errors.Is(err, ErrEVMChecksum) {
		t.Fatalf("ValidateEVM(%q) = %q, %v; want ErrEVMChecksum", typo, out, err)
	}
	if !strings.Contains(err.Error(), "cannot be recovered") {
		t.Errorf("the refusal must say what is at stake, got: %v", err)
	}
}

func TestValidateEVM_RejectsTheZeroAddress(t *testing.T) {
	out, err := ValidateEVM("0x0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrEVMZero) {
		t.Fatalf("zero address returned %q, %v; want ErrEVMZero - it is the burn hole "+
			"and what an uninitialised variable serialises to", out, err)
	}
}

func TestValidateEVM_RejectsMalformed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want error
	}{
		{"", ErrEmpty},
		{"106175F175B940CcA1816d75eB19937a88BE7720", ErrNoPrefix},
		{"0x106175F175B940CcA1816d75eB19937a88BE772z", ErrNotHex},
	} {
		if _, err := ValidateEVM(tc.in); !errors.Is(err, tc.want) {
			t.Errorf("ValidateEVM(%q) error = %v, want %v", tc.in, err, tc.want)
		}
	}
}
