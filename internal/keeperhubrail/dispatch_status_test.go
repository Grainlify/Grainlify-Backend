package keeperhubrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// These drive the REAL client against a fake KeeperHub, so the classification is
// tested where it matters: what happens to the legs.

type scripted struct {
	mu     sync.Mutex
	status []int
	bodies []string
	keys   []string
}

func (s *scripted) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Release now runs the mandatory preflight before it ever reaches the
		// webhook dispatch endpoint these tests are about, so every MCP call
		// (initialize, the notification, and execute_transfer) is answered
		// separately here, always with a clean simulation. That keeps these
		// tests exercising exactly what they say they exercise - the WEBHOOK
		// dispatch path's status classification - rather than accidentally
		// also depending on preflight passing through the same script.
		if r.URL.Path == "/mcp" {
			s.serveMCP(w, r)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		i := len(s.keys)
		s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
		if i >= len(s.status) {
			i = len(s.status) - 1
		}
		w.WriteHeader(s.status[i])
		fmt.Fprint(w, s.bodies[i])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveMCP answers every MCP call the real client's preflight simulation
// makes. Always a clean, safe simulation: these tests are about dispatch
// status classification, and preflight is expected to pass every time here.
func (s *scripted) serveMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)

	if req.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var result any
	switch {
	case req.Method == "tools/call" && req.Params.Name == "execute_transfer":
		sim, _ := json.Marshal(map[string]any{"success": true, "wouldRevert": false})
		result = map[string]any{
			"content": []map[string]string{{"type": "text", "text": string(sim)}},
			"isError": false,
		}
	default: // "initialize"
		result = map[string]any{}
	}
	resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func realClient(t *testing.T, url string) *keeperhub.Client {
	t.Helper()
	c, err := keeperhub.New("wfb_placeholder_not_a_real_key", "kh_placeholder_not_a_real_key", "wf_test")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = url
	return c
}

func ok(id string) string {
	b, _ := json.Marshal(map[string]string{"executionId": id, "status": "running"})
	return string(b)
}

// A 401 is certain: KeeperHub refused the key and ran nothing. Leaving the legs
// unknown would strand them behind a manual chain check for a condition we
// already know the answer to.
func TestRelease_ADefinitiveRejectionLeavesLegsResumable(t *testing.T) {
	f := fixture(t)
	sc := &scripted{
		status: []int{401, 200},
		bodies: []string{
			`{"error":"Invalid API key format. Expected a user webhook key starting with wfb_.","code":"invalid_key_format","expected":"wfb_*"}`,
			ok("exec-after-fix"),
		},
	}
	s := &Service{Pool: f.d.Pool, Rail: realClient(t, sc.server(t).URL)}

	if _, err := s.Release(context.Background(), f.req()); err == nil {
		t.Fatal("a 401 dispatch reported success")
	}
	for u, st := range f.legStatuses(t) {
		if st != "failed" {
			t.Fatalf("leg for %s is %q after a 401, want failed - nothing was dispatched, "+
				"so the leg is resumable without a chain check", u, st)
		}
	}

	// And it IS resumable: the next release sends the same legs, as a new attempt.
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatalf("resume after a definitive rejection: %v", err)
	}
	if len(res.LegIDs) != 3 {
		t.Errorf("resume sent %d legs, want all 3", len(res.LegIDs))
	}
	if sc.keys[0] == sc.keys[1] {
		t.Error("the resume reused the rejected attempt's idempotency key")
	}
}

// THE TRAP. A 409 idempotency_in_progress means the first request under that key
// is still running and may still pay. Treating it as a clean refusal would make
// its legs resumable, and the resume would pay them a second time.
func TestRelease_IdempotencyInProgressIsUnknown(t *testing.T) {
	f := fixture(t)
	sc := &scripted{
		status: []int{409},
		bodies: []string{`{"error":"A request with this key is in progress","code":"idempotency_in_progress","retryable":true}`},
	}
	s := &Service{Pool: f.d.Pool, Rail: realClient(t, sc.server(t).URL)}

	_, err := s.Release(context.Background(), f.req())
	if !errors.Is(err, ErrDispatchUnknown) {
		t.Fatalf("err = %v, want ErrDispatchUnknown", err)
	}
	for u, st := range f.legStatuses(t) {
		if st != "unknown" {
			t.Fatalf("leg for %s is %q after 409 idempotency_in_progress, want unknown - "+
				"the original request may still pay", u, st)
		}
	}
	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("resume after an in-progress 409: err = %v, want it blocked", err)
	}
}
