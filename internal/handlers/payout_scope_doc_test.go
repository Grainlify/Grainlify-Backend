package handlers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The doc drifted from the handlers and an INTEGRATOR found it, not a reader.
//
// docs/SCOPE-payout-endpoints.md documented a `scheme` field on
// POST /me/payout-address that the server has never bound, plus three response
// fields the claims handler does not emit. Nothing was checking, and a
// specification nobody checks is a specification that describes an earlier
// intention rather than the system.
//
// # What this asserts, and what it deliberately does not
//
// Every JSON key in the document's fenced examples must appear somewhere in the
// payout handler sources. That catches a documented field the code has no idea
// about - the whole class found here.
//
// It does NOT assert the reverse, that every key the handlers emit is
// documented. That direction needs the handler's response shape parsed properly
// rather than grepped, and a weak version of it would pass while meaning very
// little. Stated so the coverage is not overread: this checks the document
// against the code, not the code against the document.
//
// # And it has no notion of WHICH handler a key belongs to
//
// The sources are concatenated into one blob and each documented key is asked
// whether it appears anywhere in it. So a key documented under the wrong
// endpoint passes: the check answers "does this system mention this field",
// not "does THIS response carry it".
//
// Not hypothetical here. `chain_id` and `address` appear in nearly all four
// shapes in this document, so a key copy-pasted between fenced blocks - the
// most likely way this document drifts, because its blocks are edited by
// copying a neighbouring one - is exactly what this cannot see.
//
// Fixing it means associating each fenced block with its endpoint and each
// endpoint with its handler function, which is a parser rather than a grep.
// Worth doing if this file grows a fifth route; not worth pretending the
// current check covers it.
func TestPayoutScopeDocMatchesTheHandlers(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "SCOPE-payout-endpoints.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read scope doc: %v", err)
	}

	// Source is every payout handler, globbed rather than listed. A file
	// somebody forgets to add to a slice is a file the check cannot see, which
	// is the failure docs/VERIFICATION-TRAPS.md records under "a structural
	// check that enumerates its own inputs".
	matches, err := filepath.Glob("payout_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var src strings.Builder
	sources := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src.Write(b)
		sources++
	}
	// A glob that returns nothing finds no mismatches, which is
	// indistinguishable from a clean document. Assert the input before
	// trusting the output.
	if sources < 2 {
		t.Fatalf("globbed %d payout sources; the scan is not running where it thinks it is", sources)
	}
	haystack := src.String()

	// Keys inside ```json blocks only. Prose mentions field names in sentences
	// and those are not claims about the wire format.
	blocks := regexp.MustCompile("(?s)```json\\s*(.*?)```").FindAllStringSubmatch(string(doc), -1)
	if len(blocks) < 4 {
		t.Fatalf("found %d json blocks in the doc; the extraction is not working", len(blocks))
	}
	keyRe := regexp.MustCompile(`"([a-z_]+)"\s*:`)

	// Fields the document explicitly marks as specified-but-not-returned. Named
	// here so that implementing one, or removing it from the doc, has to be a
	// deliberate edit in two places rather than a silent pass.
	// contract_address left this list when #519 shipped it, which is the list
	// working: a gap that gets filled has to be removed here deliberately, and
	// removing it turns the field from exempt into asserted.
	//
	// claimed_at and deadline remain, and are answered live by
	// GET /me/claims/:id/chain rather than stored on a claim row - the chain is
	// the system of record, so a stored copy would be a cache with a staleness
	// window on the money path.
	knownGaps := map[string]bool{
		"claimed_at": true,
		"deadline":   true,
	}

	seen := map[string]bool{}
	checked := 0
	for _, b := range blocks {
		for _, m := range keyRe.FindAllStringSubmatch(b[1], -1) {
			key := m[1]
			if seen[key] || knownGaps[key] {
				continue
			}
			seen[key] = true
			checked++
			if !strings.Contains(haystack, `"`+key+`"`) {
				t.Errorf("doc documents %q but no payout handler mentions it - "+
					"either the field was removed from the code or it was never built", key)
			}
		}
	}
	if checked < 15 {
		t.Fatalf("only checked %d documented keys; the extraction is too narrow to mean anything", checked)
	}

	// The gaps must stay documented as gaps. If one is implemented, it should
	// leave this list and be asserted like every other field.
	for gap := range knownGaps {
		if !strings.Contains(string(doc), gap) {
			t.Errorf("%q is listed as a known gap but no longer appears in the doc", gap)
		}
	}
}
