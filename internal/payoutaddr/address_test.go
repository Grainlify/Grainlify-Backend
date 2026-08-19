package payoutaddr

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonical_ShortAndPaddedFormsAgree(t *testing.T) {
	long := "0x" + strings.Repeat("0", 63) + "a"
	for _, in := range []string{"0xa", "0xA", "  0xa  ", "0X0a", long, strings.ToUpper(long[:2]) + strings.ToUpper(long[2:])} {
		got, err := Canonical(in)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", in, err)
		}
		if got != long {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, long)
		}
	}
}

// The whole point of canonicalising: two spellings must not become two rows.
func TestCanonical_OneAddressHasExactlyOneRepresentation(t *testing.T) {
	a, _ := Canonical("0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9")
	b, _ := Canonical("0x1B419FE2B8C2A694EDA8398AF4BB6F6980915F9E3ED856B3B0FB4F26597F22C9")
	if a != b {
		t.Fatal("case changed the canonical form")
	}
	if len(a) != 66 {
		t.Fatalf("canonical form is %d chars, want 66", len(a))
	}
}

func TestValidate_RejectsTheReservedRange(t *testing.T) {
	for _, in := range []string{"0x0", "0x1", "0x3", "0x4", "0xf", "0x00000000000000000000000000000000000000000000000000000000000000001"[:66]} {
		if _, err := Validate(in); !errors.Is(err, ErrReserved) {
			t.Errorf("Validate(%q) = %v, want ErrReserved", in, err)
		}
	}
}

func TestValidate_AcceptsTheFirstAddressAboveTheRange(t *testing.T) {
	if _, err := Validate("0x10"); err != nil {
		t.Fatalf("0x10 is the first allowed address, got %v", err)
	}
	if _, err := Validate("0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9"); err != nil {
		t.Fatalf("a real address was rejected: %v", err)
	}
}

// The reserved check must run on the CANONICAL form. A padded 0x000...001 is
// 0x1, and a check on the string rather than the value would miss it.
func TestValidate_ReservedCheckSeesThroughPadding(t *testing.T) {
	padded := "0x" + strings.Repeat("0", 63) + "1"
	if _, err := Validate(padded); !errors.Is(err, ErrReserved) {
		t.Fatalf("a padded reserved address passed: %v", err)
	}
}

func TestCanonical_RejectsMalformed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want error
	}{
		{"", ErrEmpty},
		{"   ", ErrEmpty},
		{"0x", ErrEmpty},
		{"1b419fe2", ErrNoPrefix},
		{"0xzz", ErrNotHex},
		{"0x" + strings.Repeat("a", 65), ErrTooLong},
	} {
		got, err := Canonical(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("Canonical(%q) = %v, want %v", tc.in, err, tc.want)
		}
		// Asserted on Canonical directly, not only through Validate. Validate
		// discards this value and returns "", which masked a mutation that had
		// Canonical hand back the raw input alongside its error - so a caller
		// using Canonical on its own, or ignoring the error, would receive
		// something that looks like an address.
		if got != "" {
			t.Errorf("Canonical(%q) returned %q alongside an error", tc.in, got)
		}
	}
}

// A rejected address must not come back as something plausible.
func TestValidate_NeverNormalisesGarbageIntoAnAddress(t *testing.T) {
	for _, in := range []string{"0xzz", "not-an-address", "0x" + strings.Repeat("f", 65)} {
		got, err := Validate(in)
		if err == nil {
			t.Errorf("Validate(%q) accepted it as %q", in, got)
		}
		if got != "" {
			t.Errorf("Validate(%q) returned %q alongside an error", in, got)
		}
	}
}
