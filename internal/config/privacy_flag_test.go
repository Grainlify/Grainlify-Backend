package config

import "testing"

func TestAccountPrivacyEnabled_OnlyExactlyTrue(t *testing.T) {
	for _, v := range []string{"", "1", "TRUE", "yes", " true", "on"} {
		t.Setenv("ACCOUNT_PRIVACY_ENABLED", v)
		if Load().AccountPrivacyEnabled {
			t.Errorf("ACCOUNT_PRIVACY_ENABLED=%q switched it on", v)
		}
	}
	t.Setenv("ACCOUNT_PRIVACY_ENABLED", "true")
	if !Load().AccountPrivacyEnabled {
		t.Error(`ACCOUNT_PRIVACY_ENABLED="true" left it off`)
	}
}
