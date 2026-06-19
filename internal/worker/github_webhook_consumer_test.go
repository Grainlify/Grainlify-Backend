package worker

import "testing"

func TestDefaultQueueGroupUsesGrainlifyName(t *testing.T) {
	if DefaultQueueGroup != "grainlify-workers" {
		t.Fatalf("DefaultQueueGroup = %q, want %q", DefaultQueueGroup, "grainlify-workers")
	}
}
