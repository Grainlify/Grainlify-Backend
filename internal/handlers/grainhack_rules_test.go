package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

func rulesSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewGrainHackRulesHandler(d)
	app.Get("/grainhack/rules", h.Rules())
	return app
}

// A live hackathon's rules page must serve the frozen snapshot, not current
// config. This is the page contributors read to decide whether to trust the
// event, and §1.1's whole point is that the rules they read are the rules
// that ran - even if an admin edits global defaults afterwards.
func TestGrainHackRules_LiveHackathonServesTheFrozenSnapshot(t *testing.T) {
	d := testDB(t)
	app := rulesSuiteApp(d)
	ctx := context.Background()

	hackathonID, _, owner := asmtFxLiveIssue(t, d)

	// Freeze a snapshot containing a distinctive value.
	if _, err := d.Pool.Exec(ctx, `
UPDATE hackathons
SET config_snapshot = '{"max_concurrent_applications":"7"}'::jsonb,
    config_snapshot_taken_at = now()
WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("freeze snapshot: %v", err)
	}

	// Now change the live global default to something else. The running event
	// must not notice.
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by)
VALUES (NULL, 'max_concurrent_applications', '99', $1)
ON CONFLICT (key) WHERE hackathon_id IS NULL DO UPDATE SET value = '99'`, owner); err != nil {
		t.Fatalf("change global default: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(),
			`UPDATE hackathon_config_settings SET value = '5' WHERE hackathon_id IS NULL AND key = 'max_concurrent_applications'`)
	})

	resp, body := notifSuiteDo(t, app, "GET", "/grainhack/rules?hackathon_id="+hackathonID.String(), "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var out struct {
		Source string `json:"source"`
		Rules  []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Source != "snapshot" {
		t.Errorf("source = %q, want snapshot for a live hackathon", out.Source)
	}
	for _, r := range out.Rules {
		if r.Key == "max_concurrent_applications" {
			if r.Value == "99" {
				t.Error("the rules page served the changed global default to a live event; contributors would be reading rules that are not the ones running")
			}
			if r.Value != "7" {
				t.Errorf("max_concurrent_applications = %q, want the frozen 7", r.Value)
			}
		}
	}
}
