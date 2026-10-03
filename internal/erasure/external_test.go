package erasure

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

// The message must parse under the agent's session-action grammar
// (packages/gate/src/session-action.ts), reproduced here line for line.
var agentGrammar = regexp.MustCompile(`^Grainlify: (erase account)\nAction: ([a-z][a-z0-9_]{0,63})\nGitHub: ([A-Za-z0-9](?:[A-Za-z0-9-]{0,38})) \(id ([1-9][0-9]{0,15})\)\nSubject: ([A-Za-z0-9_.:/-]{0,128})\nNonce: ([0-9a-f]{32})\nIssued: (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\nExpires: (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)$`)

func TestAgentErasure_SignedUnderItsOwnDomain(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	key := ed25519.NewKeyFromSeed(seed)
	var got struct{ Message, Countersignature string }
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/erase" || r.Method != "POST" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"in_flight"}`))
	}))
	defer srv.Close()

	s := NewServices("", "", "", srv.URL, base64.StdEncoding.EncodeToString(seed))
	s.Now = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
	if err := s.EraseAtAgent(context.Background(), 42, "ada", false); err != nil {
		t.Fatalf("EraseAtAgent: %v", err)
	}
	m := agentGrammar.FindStringSubmatch(got.Message)
	if m == nil {
		t.Fatalf("message does not parse under the agent's grammar:\n%s", got.Message)
	}
	if m[2] != "erase" || m[3] != "ada" || m[4] != "42" || m[5] != "" {
		t.Errorf("fields = %q", m[1:])
	}
	if m[8] != "2026-10-03T12:10:00Z" {
		t.Errorf("expires = %s, want ten minutes after issue", m[8])
	}
	sig, _ := base64.StdEncoding.DecodeString(got.Countersignature)
	pub := key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, []byte(AgentErasureDomain+got.Message), sig) {
		t.Error("signature does not verify under the erasure domain")
	}
	for _, other := range []string{"grainlify-bounty-apply:v1\n", "grainlify-bounty-admin:v1\n", "grainlify-bounty-maintainer:v1\n"} {
		if ed25519.Verify(pub, []byte(other+got.Message), sig) {
			t.Errorf("an erasure signature verifies under %q", other)
		}
	}

	// Past the hold limit the action itself changes, so it is signed too.
	if err := s.EraseAtAgent(context.Background(), 42, "ada", true); err != nil {
		t.Fatalf("EraseAtAgent retaining: %v", err)
	}
	if m := agentGrammar.FindStringSubmatch(got.Message); m == nil || m[2] != AgentActionEraseRetaining {
		t.Errorf("retaining erasure sent action %q, want %q", m, AgentActionEraseRetaining)
	}

	status = http.StatusConflict
	if err := s.EraseAtAgent(context.Background(), 42, "ada", false); !errors.Is(err, ErrAgentInFlight) {
		t.Errorf("409 = %v, want ErrAgentInFlight", err)
	}
	status = http.StatusInternalServerError
	if err := s.EraseAtAgent(context.Background(), 42, "ada", false); err == nil || errors.Is(err, ErrAgentInFlight) {
		t.Errorf("500 = %v, want a plain failure", err)
	}
}

func TestServices_UnconfiguredStepsSaySo(t *testing.T) {
	s := NewServices("", "", "", "", "")
	if err := s.EraseAtAgent(context.Background(), 1, "a", false); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("agent: %v", err)
	}
	if err := s.DeleteDiditSession(context.Background(), "x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("didit: %v", err)
	}
	if err := s.RevokeGitHub(context.Background(), "t"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("github: %v", err)
	}
}
