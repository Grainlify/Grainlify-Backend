package config

// The background-work contract between cmd/api and cmd/worker.
//
// # The silent configuration this exists to make loud
//
// cmd/api runs the background workers - the sync-job runner, GrainHack's
// reconciliation crawl, the KYC review sweep, the KYC status reconciler - only
// when NATS_URL is empty:
//
//	if cfg.NATSURL == "" && database != nil { ...start them... }
//	else { slog.Info("background worker skipped", "reason", "NATS configured (use external worker)") }
//
// That log line describes a worker process that does not exist. cmd/worker is
// fourteen lines and prints "worker is not implemented in this build". So with
// NATS_URL set, every one of those jobs runs NOWHERE, and the only trace is an
// INFO line saying something else is handling it.
//
// This is precisely the class the boot gate in required.go was built for: a
// live feature silently dead because of one environment variable, with nothing
// logged as wrong. It is arguably worse than an empty variable, because the
// value is present and the log reads like a deliberate architecture.
//
// # Why a constant rather than a health check
//
// Whether an external worker is RUNNING is a runtime question and would need a
// heartbeat. Whether one has been WRITTEN is a fact about this build, and this
// is that fact. Flip it in one place when cmd/worker does something, and the
// refusal below disappears with it.
//
// cmd/worker reads the same constant, so the two cannot drift into disagreeing
// about whether it is implemented.
const ExternalWorkerImplemented = false

// BackgroundWorkHasNowhereToRun reports whether this configuration would leave
// the background jobs unrun, and the message a human reads if it does.
//
// Refusing to start is the same trade required.go documents and for the same
// reason: Railway keeps the previous deployment serving when a new one fails
// its healthcheck, so a refusal is loud and immediate without being an outage.
// The deploy stops; the site does not.
//
// Dev is exempt on the same principle as GatesBoot - a developer pointing at a
// local NATS should not be stopped from running the API.
func (c Config) BackgroundWorkHasNowhereToRun() (bool, string) {
	if !c.GatesBoot() || ExternalWorkerImplemented || c.NATSURL == "" {
		return false, ""
	}
	return true, "refusing to start: NATS_URL is set, which makes cmd/api skip the background workers " +
		"in favour of an external worker - and cmd/worker is not implemented in this build.\n" +
		"  affected: sync-job runner, GrainHack reconciliation crawl, KYC review sweep, KYC status reconciler\n" +
		"  with NATS_URL set these run in no process at all, and the only trace is an INFO line saying otherwise.\n" +
		"unset NATS_URL to run them in-process, or implement cmd/worker and set ExternalWorkerImplemented; " +
		"the previous deployment keeps serving until this one boots"
}
