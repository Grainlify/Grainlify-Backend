package config

import "testing"

func TestBackgroundWorkHasNowhereToRun(t *testing.T) {
	// The case that motivated this: production sets NATS_URL, cmd/api skips
	// the background workers "in favour of the external worker", and no
	// external worker exists.
	t.Run("refuses when NATS_URL is set and no worker is implemented", func(t *testing.T) {
		c := Config{Env: "production", NATSURL: "nats://example:4222"}
		nowhere, msg := c.BackgroundWorkHasNowhereToRun()
		if !nowhere {
			t.Fatal("did not refuse; the background jobs would run in no process at all")
		}
		// The message has to name what stops running, or a deploy failure is
		// just an obstacle rather than an explanation.
		for _, want := range []string{"NATS_URL", "cmd/worker", "KYC status reconciler"} {
			if !contains(msg, want) {
				t.Errorf("message does not mention %q: %s", want, msg)
			}
		}
	})

	t.Run("permits when NATS_URL is empty - the jobs run in-process", func(t *testing.T) {
		c := Config{Env: "production"}
		if nowhere, _ := c.BackgroundWorkHasNowhereToRun(); nowhere {
			t.Error("refused with NATS_URL unset, which is the configuration that works")
		}
	})

	// Same principle as GatesBoot: a developer pointing at a local NATS must
	// not be stopped from running the API.
	t.Run("permits in dev", func(t *testing.T) {
		c := Config{Env: "dev", NATSURL: "nats://localhost:4222"}
		if nowhere, _ := c.BackgroundWorkHasNowhereToRun(); nowhere {
			t.Error("refused in dev")
		}
	})

	// The guard must disappear on its own when the worker is written, rather
	// than needing to be remembered. If this ever fails, ExternalWorkerImplemented
	// has been flipped and the refusal above is now dead code to delete.
	t.Run("the constant is what turns the refusal off", func(t *testing.T) {
		if ExternalWorkerImplemented {
			c := Config{Env: "production", NATSURL: "nats://example:4222"}
			if nowhere, _ := c.BackgroundWorkHasNowhereToRun(); nowhere {
				t.Fatal("ExternalWorkerImplemented is true but the refusal still fires")
			}
			t.Log("cmd/worker is implemented; delete BackgroundWorkHasNowhereToRun and this test")
		}
	})
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
