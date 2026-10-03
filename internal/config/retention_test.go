package config

import "testing"

// The retention job deletes, so it is on only for the exact value "true".
// Anything else - unset, a typo, or a truthy spelling getEnvBool would accept -
// leaves it off.
func TestRetentionJobEnabled_OnlyForExactlyTrue(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"true", true},
		{"TRUE", false},
		{"True", false},
		{" true", false},
		{"1", false},
		{"yes", false},
		{"on", false},
		{"false", false},
	} {
		t.Setenv("RETENTION_JOB_ENABLED", tc.value)
		if got := Load().RetentionJobEnabled; got != tc.want {
			t.Errorf("RETENTION_JOB_ENABLED=%q: enabled = %v, want %v", tc.value, got, tc.want)
		}
	}
}
