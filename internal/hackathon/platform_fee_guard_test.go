package hackathon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoMoneyPathReadsTheSponsorTotal is the structural guarantee behind the
// whole design.
//
// contributor_prize_pool and maintainer_prize_pool hold NET amounts: the
// platform fee is taken once, at record time, and never subtracted again
// anywhere downstream. That is what keeps every payout path correct by
// construction rather than correct because each of fourteen call sites was
// found and edited.
//
// The failure mode it protects against is a *new* reader. Somebody adding a
// payout or settlement path next year, who never saw this decision, reaches
// for the field that sounds like the pool - sponsor_total_usdc - and silently
// pays out the fee. Nothing else would catch it: the arithmetic still
// balances, the tests still pass, and the only symptom is that the platform's
// fee went to contributors.
//
// So: money paths may not name the gross fields at all. The places that
// legitimately handle gross are the record-time split, the publishing
// surfaces, and the audit snapshot - each listed explicitly, so adding a
// fifteenth is a visible edit rather than a silent one.
func TestNoMoneyPathReadsTheSponsorTotal(t *testing.T) {
	// Gross-only identifiers. The fee columns count too: a payout path with
	// any reason to look at the fee is a payout path doing fee arithmetic,
	// which is what taking it once at record time exists to avoid.
	forbidden := []string{
		"sponsor_total_usdc",
		"SponsorTotalUSDC",
		"platform_fee_usdc",
		"PlatformFeeUSDC",
		"SplitSponsorTotal",
	}

	// Files allowed to handle gross, each for a stated reason.
	allowed := map[string]string{
		"platform_fee.go":            "defines the split itself",
		"platform_fee_test.go":       "tests the split",
		"platform_fee_guard_test.go": "this guard",
		"config.go":                  "declares the fee rate keys",
		"admin_hackathons.go":        "applies the fee at record time - the one write path",
		"hackathon_public.go":        "publishes total, fee and net together",
		"grainhack_rules.go":         "publishes the breakdown on the rules page",
		"appeals.go":                 "records gross and fee on the payout run snapshot",
		"admin_hackathons_test.go":   "tests the record-time split",
		"hackathon_public_test.go":   "tests the published breakdown",
	}

	// Everywhere money is divided, allocated, escrowed or settled.
	roots := []string{".", "../handlers", "../founding", "../chain", "../syncjobs"}

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if reason, ok := allowed[filepath.Base(path)]; ok {
				t.Logf("allowed: %s (%s)", filepath.Base(path), reason)
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(b)
			for _, name := range forbidden {
				if !strings.Contains(src, name) {
					continue
				}
				t.Errorf("%s references %q.\n"+
					"contributor_prize_pool and maintainer_prize_pool already hold NET amounts - the platform fee "+
					"was taken once at record time. A money path reading the gross total is paying out the fee, and "+
					"the arithmetic will still balance, so nothing else will catch it. Use the net pool columns. If "+
					"this file legitimately publishes or audits the gross figure, add it to the allowed list with a reason.",
					path, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
