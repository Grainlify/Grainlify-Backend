// Package dbguard refuses to run a destructive command against a database that
// is not local, unless the operator names the host.
//
// # Why this is a package and not a copy
//
// Two commands need it - cmd/migrate and cmd/payout - and a second copy of a
// safety check is a check that drifts. One of them gets a fix, the other does
// not, and the one that did not is discovered by somebody who trusted it.
//
// # Why the flag names the host
//
// A bare confirmation flag gets pasted out of shell history or a runbook and
// approves whatever database happens to be configured now. One that names the
// host cannot be reused against a different one: it fails, loudly, naming both.
//
// # Why the stakes differ between callers
//
// A migration against the wrong database is recoverable - the schema changes and
// you migrate back. `payout persist` and `payout build` against the wrong one
// produce a settlement and a Merkle tree for an event that does not exist there,
// and a `publish_root` funded against that tree moves real money on the strength
// of it. The guard is identical; the consequence is not.
package dbguard

import (
	"fmt"
	"net/url"
	"strings"
)

// ConfirmFlag is the only way past this guard.
const ConfirmFlag = "--yes-run-against-remote-host"

// localHosts are the only hosts that need no confirmation.
var localHosts = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true,
	"host.docker.internal": true,
}

// hostOf extracts the host from either a URL-style DB_URL or a keyword DSN.
//
// It returns an error rather than a guess. An unparseable DB_URL must not be
// treated as local: that would make the one case we cannot reason about the one
// case that runs unguarded, which is the shape of every entry in
// docs/VERIFICATION-TRAPS.md.
// HostOf is exported so a caller can name the target in its own output.
func HostOf(dbURL string) (string, error) {
	s := strings.TrimSpace(dbURL)
	if s == "" {
		return "", fmt.Errorf("DB_URL is empty")
	}
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("DB_URL is not a parseable URL: %w", err)
		}
		h := u.Hostname()
		if h == "" {
			return "", fmt.Errorf("DB_URL has no host")
		}
		return h, nil
	}
	// Keyword DSN: host=... user=... — a unix socket has no host or a path one.
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "host=") {
			h := strings.TrimPrefix(field, "host=")
			if h == "" {
				return "", fmt.Errorf("DB_URL sets an empty host=")
			}
			if strings.HasPrefix(h, "/") {
				return "localhost", nil // unix socket is by definition this machine
			}
			return h, nil
		}
	}
	return "", fmt.Errorf("could not find a host in DB_URL")
}

func isLocal(host string) bool { return localHosts[strings.ToLower(host)] }

// confirmedHost returns the host named by the confirmation flag, if present.
func confirmedHost(args []string) (string, bool) {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, ConfirmFlag+"="); ok {
			return v, true
		}
		if a == ConfirmFlag && i+1 < len(args) {
			return args[i+1], true
		}
		if a == ConfirmFlag {
			return "", true // present but with no value: seen, and will not match
		}
	}
	return "", false
}

// checkTarget decides whether this run may proceed. The error it returns is
// shown to the operator, so it names the host and never the URL - DB_URL
// carries a password.
// Check refuses unless the target is local or the operator named the host.
//
// `what` names the command in the refusal, so the message says which act was
// stopped rather than leaving the reader to infer it.
func Check(what, dbURL string, args []string) error {
	host, err := HostOf(dbURL)
	if err != nil {
		return fmt.Errorf("refusing to run %s: %w. Fix DB_URL, or run against a local database", what, err)
	}
	if isLocal(host) {
		return nil
	}
	named, present := confirmedHost(args)
	if !present {
		return fmt.Errorf(
			"refusing to run %s against %q: it is not a local database.\n\n"+
				"This is very likely production. If you meant it, name the host:\n\n"+
				"    %s\n\n"+
				"This is very likely production.",
			what, host, fmt.Sprintf("%s %s=%s", what, ConfirmFlag, host))
	}
	if named != host {
		return fmt.Errorf(
			"refusing to run: the confirmation names a different database.\n\n"+
				"    you confirmed: %q\n"+
				"    DB_URL is:     %q\n\n"+
				"That mismatch is the point of naming the host - a confirmation copied "+
				"from elsewhere cannot approve this database. Check which one you meant.",
			named, host)
	}
	return nil
}
