package founding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSettlementFiguresNeverReachAPresentationLayer is the enforcement of the
// one hard constraint on the settlement computation: **no computed dollar
// figure may reach any UI or notification.**
//
// §6 of the redesign forbids publishing any per-person figure. A settlement
// result leaking into a profile page, an API response or a notification body
// would publish exactly that - and it would do so as an apparent promise,
// before any disbursement path exists to honour it. The announcement can
// promise a pool; it must never imply a personal amount.
//
// This is a source scan rather than a runtime assertion because the failure
// mode is somebody *adding* a handler later. A runtime test can only cover
// endpoints that exist; this fails the moment the settlement tables or the
// money-carrying field names appear anywhere that renders to a person.
func TestSettlementFiguresNeverReachAPresentationLayer(t *testing.T) {
	// Where a value would become visible to a user.
	presentationRoots := []string{"../handlers", "../notifications", "../api"}

	// Names that only exist to carry a computed amount. The share ledger and
	// wave tables are deliberately absent: share counts and wave membership
	// are publishable, and /founding/me returns them.
	forbidden := []string{
		"founding_settlements",
		"founding_settlement_lines",
		"share_value_usdc",
		"usdc_amount",
		"ShareValueUSDC",
		"USDCAmount",
	}

	for _, root := range presentationRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(b)
			// redemptions.go is the retired points programme's own handler.
			// It carries usdc_amount / USDCAmount for balances that predate
			// this rule, and it is frozen - Create refuses outright, so it
			// renders a figure for nothing that can still be earned. The
			// settlement table names below are still checked here, because a
			// founding figure appearing in it would be a genuine leak.
			isRetiredPointsHandler := strings.HasSuffix(path, "redemptions.go")

			for _, name := range forbidden {
				if !strings.Contains(src, name) {
					continue
				}
				if isRetiredPointsHandler && (name == "usdc_amount" || name == "USDCAmount") {
					continue
				}
				t.Errorf("%s references %q.\n"+
					"A computed settlement figure must never reach a UI or a notification (§6): it is a "+
					"per-person number, and there is no disbursement path to honour it. Store it; render nothing.",
					path, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
