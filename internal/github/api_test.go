package github

import "testing"

func TestNewClientUsesGrainlifyUserAgent(t *testing.T) {
	client := NewClient()
	if client.UserAgent != "grainlify-backend" {
		t.Fatalf("UserAgent = %q, want %q", client.UserAgent, "grainlify-backend")
	}
}
