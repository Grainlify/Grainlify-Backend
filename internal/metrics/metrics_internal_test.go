package metrics

import "testing"

// TestSecureCompare is the regression test for issue #300: TokenGate must
// use a constant-time comparison, not ==, for the METRICS_TOKEN check.
// TestTokenGate/TestTokenGate_EmptyToken_Passthrough in metrics_test.go
// already cover TokenGate's end-to-end behavior (wrong/correct/empty
// token); this covers secureCompare itself directly, including the
// mismatched-length case that a naive constant-time compare over raw
// (unhashed) inputs would otherwise panic or behave inconsistently on.
func TestSecureCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "equal strings", a: "secret-token", b: "secret-token", want: true},
		{name: "different strings, same length", a: "secret-token", b: "secret-tokeX", want: false},
		{name: "different strings, different length", a: "secret-token", b: "short", want: false},
		{name: "both empty", a: "", b: "", want: true},
		{name: "one empty", a: "secret-token", b: "", want: false},
		{name: "case-sensitive", a: "Secret-Token", b: "secret-token", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := secureCompare(tc.a, tc.b); got != tc.want {
				t.Errorf("secureCompare(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
