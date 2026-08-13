package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

type publishedRule struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	Unenforced string `json:"unenforced"`
}

func fetchPublishedRules(t *testing.T, d *db.DB) []publishedRule {
	t.Helper()
	app := fiber.New()
	app.Get("/grainhack/rules", handlers.NewGrainHackRulesHandler(d).Rules())

	resp, err := app.Test(httptest.NewRequest("GET", "/grainhack/rules", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Rules []publishedRule `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Rules
}

// TestRules_ContractEnforcedSettingsPublishNoNumber is the guard for a class of
// defect this project has now hit twice.
//
// The referral window was published as "30 days" from a frontend constant while
// the backend enforced its own; the fix was to serve the published figure from
// the constant that enforces it. unclaimed_sweep_days is the same shape and
// worse: the timelock is fixed inside the escrow contract at initialise(), and
// nothing connects the config row to it. Publishing 180 would state a rule that
// no deployed code enforces - and there is no deployed escrow at all.
//
// So a setting marked EnforcedOnChain publishes its key, its description and
// the reason, and no number. The rule stays visible; the figure does not appear
// until it can be read back from the contract that enforces it.
func TestRules_ContractEnforcedSettingsPublishNoNumber(t *testing.T) {
	d := testDB(t)
	rules := fetchPublishedRules(t, d)

	byKey := map[string]publishedRule{}
	for _, r := range rules {
		byKey[r.Key] = r
	}

	found := 0
	for key, def := range hackathon.Definitions {
		if !def.EnforcedOnChain {
			continue
		}
		found++
		got, ok := byKey[key]
		if !ok {
			t.Errorf("%s is not published at all; it should appear with its description and no value", key)
			continue
		}
		if got.Value != "" {
			t.Errorf("%s published the value %q, which no deployed contract enforces", key, got.Value)
		}
		if got.Unenforced == "" {
			t.Errorf("%s published a blank value with no explanation; a reader cannot tell that from a missing setting", key)
		}
	}

	if found == 0 {
		t.Fatal("no EnforcedOnChain settings found; this guard is asserting nothing - if the flag was removed, remove this test deliberately")
	}
}

// TestRules_OrdinarySettingsStillPublishTheirValue is the other half: the
// suppression must be narrow. A rule the backend really does enforce has to
// keep showing its number, or the page stops being useful.
func TestRules_OrdinarySettingsStillPublishTheirValue(t *testing.T) {
	d := testDB(t)
	rules := fetchPublishedRules(t, d)

	var checked int
	for _, r := range rules {
		def, ok := hackathon.Definitions[r.Key]
		if !ok || def.EnforcedOnChain {
			continue
		}
		if r.Value == "" && def.Default != "" {
			t.Errorf("%s published no value, but nothing marks it unenforced", r.Key)
		}
		if r.Unenforced != "" {
			t.Errorf("%s carries an unenforced reason but is not marked EnforcedOnChain", r.Key)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no ordinary settings were checked; the fixture is not exercising the endpoint")
	}
}
