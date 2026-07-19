package handlers

import "testing"

func TestMapDiditStatusTerminalDecisions(t *testing.T) {
	tests := []struct {
		name        string
		diditStatus string
		want        string
	}{
		{name: "approved", diditStatus: "approved", want: "verified"},
		{name: "verified", diditStatus: "verified", want: "verified"},
		{name: "rejected", diditStatus: "rejected", want: "rejected"},
		{name: "declined", diditStatus: "declined", want: "rejected"},
		{name: "expired", diditStatus: "expired", want: "expired"},
		{name: "unknown fails closed", diditStatus: "ambiguous_provider_status", want: "not_started"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapDiditStatus(tt.diditStatus); got != tt.want {
				t.Fatalf("mapDiditStatus(%q) = %q, want %q", tt.diditStatus, got, tt.want)
			}
		})
	}
}
