package handlers

import (
	"context"

	"github.com/google/uuid"
)

// A place a support request gets delivered to, after it has been written down.
//
// This exists so Discord stops being the system of record. The endpoint used
// to build a Discord multipart request inline and return 502 when it failed,
// which meant the report only ever existed as an in-flight HTTP call: if the
// webhook was down, whatever the person had typed was gone.
//
// Sinks are peers and are attempted independently. One being down must not
// stop another, and neither must lose the row - delivery is recorded per sink
// on support_requests so a failed one can be identified and replayed instead
// of guessed at.
type SupportSink interface {
	// Name identifies the sink in logs and in the delivery column it owns.
	Name() string
	// Deliver sends one request. An error means "not delivered": the caller
	// records nothing and moves on to the next sink.
	//
	// The result describes HOW it was delivered, which is not always "as
	// intended" - a topic post that had to fall back to General reached a
	// human but was misrouted, and that must be recorded rather than look
	// identical to a clean delivery.
	Deliver(ctx context.Context, req SupportRequest) (SupportDeliveryResult, error)
	// Configured reports whether this sink has what it needs to run. An
	// unconfigured sink is skipped quietly rather than counted as a failure -
	// Telegram not being set up is not a Discord outage.
	Configured() bool
	// Handles reports whether this sink should receive this category at all.
	//
	// This exists so a category can be excluded by code rather than by
	// configuration. Discord returns false for "kyc": the alternative was
	// keeping the Discord channel private and trusting that it stays private,
	// which makes the privacy of a verification request a property of a
	// permission setting that anybody with Manage Channels can change without
	// it being noticed. A sink that never receives the category cannot leak it
	// whatever the channel is set to.
	//
	// Not handling a category is not a failure and not a delivery. The row's
	// column for that sink stays NULL for ever, which is why supportDelivered
	// has to know about it too - otherwise every KYC row looks permanently
	// undelivered to any replay.
	Handles(category string) bool
}

// SupportDeliveryResult describes a successful delivery.
type SupportDeliveryResult struct {
	// RoutedToFallback means the message reached the chat but not the topic it
	// was addressed to - the topic was deleted, or the bot lost
	// can_manage_topics. Recorded on the row because reports quietly piling
	// into General while a topic sits empty is invisible otherwise.
	RoutedToFallback bool
}

// SupportRequest is one submitted request, as stored.
//
// WHAT IS DELIBERATELY NOT HERE: the reporter's IP address, and any
// client-supplied claim of identity. The IP is recorded on the row for abuse
// investigation and must not reach any sink - the Discord payload had been
// including it, and a public Telegram topic must never see it. Identity is a
// UserID resolved from the JWT, so a sink can look up a login itself if it is
// entitled to; it can no longer be told one by the browser.
type SupportRequest struct {
	ID       uuid.UUID
	Category string
	Message  string
	PageURL  string
	// UserAgent is a browser string, not a person. Useful for reproducing a
	// bug and safe to forward.
	UserAgent string
	// UserID is nil for anonymous reports, which are legitimate: somebody who
	// cannot sign in is exactly the person most likely to need support.
	UserID *uuid.UUID
	// ReporterLogin is resolved server-side from UserID, empty when anonymous.
	// Sinks that post somewhere public must still redact it - having it here
	// means a sink CAN identify the reporter, not that it should.
	ReporterLogin string

	ScreenshotBytes []byte
	ScreenshotExt   string
}
