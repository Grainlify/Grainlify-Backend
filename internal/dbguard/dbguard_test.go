package dbguard

import "testing"

const prodURL = "postgres://neondb_owner:npg_SUPERSECRET@ep-cool-name.eu-central-1.aws.neon.tech/neondb?sslmode=require"

func TestCheckTarget_LocalNeedsNoConfirmation(t *testing.T) {
	for _, u := range []string{
		"postgres://user:pass@localhost:5432/grainlify?sslmode=disable",
		"postgres://user:pass@127.0.0.1:5432/grainlify",
		"postgres://user:pass@[::1]:5432/grainlify",
		"host=localhost user=grainlify dbname=grainlify",
		"host=/var/run/postgresql dbname=grainlify",
	} {
		if err := Check("cmd/migrate", u, nil); err != nil {
			t.Errorf("local database refused: %v\n  url: %s", err, u)
		}
	}
}

func TestCheckTarget_RemoteIsRefusedWithoutTheFlag(t *testing.T) {
	err := Check("cmd/migrate", prodURL, nil)
	if err == nil {
		t.Fatal("a remote database was migrated with no confirmation")
	}
}

func TestCheckTarget_RemoteProceedsWhenTheHostIsNamed(t *testing.T) {
	args := []string{"--yes-run-against-remote-host=ep-cool-name.eu-central-1.aws.neon.tech"}
	if err := Check("cmd/migrate", prodURL, args); err != nil {
		t.Fatalf("a correctly confirmed run was refused: %v", err)
	}
	// The space-separated form has to work too, or the message we print is wrong.
	if err := Check("cmd/migrate", prodURL, []string{"--yes-run-against-remote-host", "ep-cool-name.eu-central-1.aws.neon.tech"}); err != nil {
		t.Fatalf("space-separated confirmation was refused: %v", err)
	}
}

// The reason the flag takes the host as its value rather than being a bare
// --yes: a confirmation pasted out of shell history must not approve a database
// it was never typed for.
func TestCheckTarget_AConfirmationForAnotherHostDoesNotApproveThisOne(t *testing.T) {
	args := []string{"--yes-run-against-remote-host=staging.example.com"}
	err := Check("cmd/migrate", prodURL, args)
	if err == nil {
		t.Fatal("a confirmation naming a DIFFERENT host approved this database")
	}
}

func TestCheckTarget_BareFlagWithNoValueDoesNotConfirm(t *testing.T) {
	if err := Check("cmd/migrate", prodURL, []string{"--yes-run-against-remote-host"}); err == nil {
		t.Fatal("the flag with no value approved a remote database")
	}
	if err := Check("cmd/migrate", prodURL, []string{"--yes-run-against-remote-host="}); err == nil {
		t.Fatal("the flag with an empty value approved a remote database")
	}
}

// An unparseable DB_URL must fail closed. Treating "cannot tell" as "local"
// would make the one case we cannot reason about the one case that runs
// unguarded.
func TestCheckTarget_FailsClosedWhenTheHostCannotBeDetermined(t *testing.T) {
	for _, u := range []string{"", "   ", "not a url at all", "mysql://user@host/db", "dbname=grainlify"} {
		if err := Check("cmd/migrate", u, nil); err == nil {
			t.Errorf("an undeterminable DB_URL was allowed to migrate: %q", u)
		}
	}
}

// DB_URL carries a password. It must never reach the operator's terminal, where
// it lands in scrollback and in whatever they paste into a bug report.
func TestCheckTarget_TheErrorNeverLeaksTheCredential(t *testing.T) {
	for _, args := range [][]string{nil, {"--yes-run-against-remote-host=elsewhere.example.com"}} {
		err := Check("cmd/migrate", prodURL, args)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		for _, secret := range []string{"npg_SUPERSECRET", "neondb_owner", prodURL} {
			if contains(err.Error(), secret) {
				t.Errorf("the refusal leaked %q:\n%s", secret, err.Error())
			}
		}
	}
}

func TestHostOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"postgres://u:p@example.com:5432/db", "example.com"},
		{"postgresql://u:p@example.com/db", "example.com"},
		{"postgres://u:p@[::1]:5432/db", "::1"},
		{"host=example.com port=5432", "example.com"},
		{"host=/var/run/postgresql", "localhost"},
	} {
		got, err := HostOf(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("HostOf(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
