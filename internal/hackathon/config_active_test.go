package hackathon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every config key's Active flag must match reality: Active means some Go
// code actually reads it, and inactive means none does.
//
// Both directions have already gone wrong here, in ways that are worse than a
// missing feature:
//
//   - auto_revert_oob_assignment was seeded, listed in the admin UI as
//     active, and read by nothing. An admin could switch it off, see it off,
//     and Grainlify carried on removing assignees and commenting on
//     repositories it does not own.
//   - maintainer_criteria_weights was the inverse: it worked end to end but
//     was labelled inert, so the rules page told people a knob did nothing
//     when it did.
//
// Four keys hit one or the other of these during this build, which is why
// this is a test rather than a note asking the next person to check.
func TestConfigDefinitions_ActiveFlagMatchesActualUse(t *testing.T) {
	roots := []string{".", "../handlers", "../syncjobs", "../api", "../ingest"}

	// Read every non-test Go file once; keys are then matched against the
	// whole corpus rather than re-walking per key.
	var corpus strings.Builder
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// config.go is the definition site; a key appearing only there is
			// declared, not consumed.
			if filepath.Base(path) == "config.go" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err == nil {
				corpus.Write(b)
			}
			return nil
		})
	}
	src := corpus.String()

	var lyingActive, understatedInactive []string
	for key, def := range Definitions {
		referenced := strings.Contains(src, `"`+key+`"`)
		switch {
		case def.Active && !referenced:
			lyingActive = append(lyingActive, key)
		case !def.Active && referenced:
			understatedInactive = append(understatedInactive, key)
		}
	}

	if len(lyingActive) > 0 {
		t.Errorf("these keys are marked Active but no Go code reads them. An admin will set one, "+
			"see it set, and believe it took effect:\n  %s", strings.Join(lyingActive, "\n  "))
	}
	if len(understatedInactive) > 0 {
		t.Errorf("these keys are read by Go code but marked inactive, so the rules page tells people "+
			"they do nothing:\n  %s", strings.Join(understatedInactive, "\n  "))
	}
}
