package calibration

import "testing"

// TestCombinedPromptCannotBeConfusedWithProduction is the namespace guard.
//
// A run recorded under one prompt must never be readable as the other. They
// answer different questions: production's judge assesses quality on pull
// requests that have ALREADY passed a deterministic coordination gate, while
// the combined prompt asks one model to do both jobs because the calibration
// set never went through that gate.
func TestCombinedPromptCannotBeConfusedWithProduction(t *testing.T) {
	combinedV, combinedSHA := CombinedPromptFingerprint()
	prodV, prodSHA := ProductionJudgingFingerprint()

	if combinedSHA == prodSHA {
		t.Fatal("the two prompts are the same text; the combined prompt is meant to state coordination rules production enforces in code")
	}
	if combinedV == prodV {
		t.Fatal("the two versions collide")
	}
	if got := combinedV[:len("calibration-combined@")]; got != "calibration-combined@" {
		t.Errorf("combined namespace = %q, want calibration-combined@", got)
	}
	if got := prodV[:len("hackathon-judging@")]; got != "hackathon-judging@" {
		t.Errorf("production namespace = %q, want hackathon-judging@", got)
	}
	// And the shadow runner must stamp the combined one.
	runV, _ := PromptFingerprint()
	if runV != combinedV {
		t.Errorf("shadow runs stamp %q, want the combined prompt %q", runV, combinedV)
	}
}

// The combined prompt must actually state the coordination rules, or it is not
// doing the job it exists for.
func TestCombinedPromptStatesTheCoordinationRules(t *testing.T) {
	p := CombinedPrompt()
	for _, want := range []string{"not linked to an issue", "documentation only", "no meaningful code", "does not do what the linked issue asked"} {
		if !contains(p, want) {
			t.Errorf("combined prompt does not state %q", want)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
