// Package email sends transactional emails. The rest of the codebase only
// depends on the Mailer interface, so the provider is swappable without
// touching any caller.
package email

import "context"

// Mailer sends a single HTML email. Implementations should be safe for
// concurrent use.
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}
