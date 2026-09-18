package keeperhubrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// realSimRail is fakeRail with one difference: SimulateTransfer goes through
// the real keeperhub.Client, against a server answering the way a live
// KeeperHub instance does. Every other preflight test hands WouldRevert back
// from a fake, which is exactly how the client's handling of a real revert
// went untested - the fake returned a result the real API never sends.
type realSimRail struct {
	*fakeRail
	kh *keeperhub.Client
}

func (r *realSimRail) SimulateTransfer(ctx context.Context, chainID, to, amount, token string) (keeperhub.TransferSimulation, error) {
	return r.kh.SimulateTransfer(ctx, chainID, to, amount, token)
}

func TestRelease_ARealRevertFromKeeperHubRefusesAsWouldRevert(t *testing.T) {
	f := fixture(t)
	fake := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: fake}
	_, _, _, sendable, err := s.previewRun(context.Background(), f.req())
	if err != nil {
		t.Fatalf("previewRun: %v", err)
	}
	target := strings.ToLower(sendable[1].Address)

	// Captured verbatim from KeeperHub: a transfer that would revert arrives
	// as isError with an HTTP 400 description, not as a normal result.
	revert, err := os.ReadFile("../keeperhub/testdata/simulate_revert_iserror.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] != "tools/call" {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		args := body["params"].(map[string]any)["arguments"].(map[string]any)
		result := map[string]any{"content": []any{map[string]any{"text": `{"success":true,"wouldRevert":false,"error":""}`}}}
		if strings.ToLower(args["to_address"].(string)) == target {
			result = map[string]any{"isError": true, "content": []any{map[string]any{"text": strings.TrimRight(string(revert), "\n")}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": result})
	}))
	t.Cleanup(srv.Close)

	kh, err := keeperhub.New("wfb_placeholder_not_a_real_key", "kh_placeholder_not_a_real_key", "wf_probe")
	if err != nil {
		t.Fatal(err)
	}
	kh.BaseURL = srv.URL
	s.Rail = &realSimRail{fakeRail: fake, kh: kh}

	_, err = s.Release(context.Background(), f.req())
	if errors.Is(err, ErrPreflightUnavailable) {
		t.Fatalf("a leg KeeperHub said would revert was reported as unavailable - the bug this pins: %v", err)
	}
	if !errors.Is(err, ErrPreflightWouldRevert) {
		t.Fatalf("err = %v, want ErrPreflightWouldRevert", err)
	}
	if !strings.Contains(err.Error(), "exceeds balance") {
		t.Errorf("the refusal should carry KeeperHub's reason, got %v", err)
	}
	if len(fake.calls) != 0 {
		t.Error("a release refused by preflight reached Dispatch")
	}
}
