package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// BountyEventsHandler turns things that happened in the bounty agent into
// notifications here.
//
// The agent knows the facts and owns none of the machinery for telling anyone:
// email, in-app notifications and a person's preferences all live in this
// service. So it queues events and posts them here.
//
// Authenticated by HMAC over the exact body, the same shape as the GitHub
// webhook this service already verifies. A shared secret rather than the
// ed25519 pair used for the other direction, because that pair only runs one
// way - this service holds the private half, the agent has the public one and
// can verify our signatures without being able to make its own.
type BountyEventsHandler struct {
	db     *db.DB
	notif  *notifications.Service
	secret string
}

func NewBountyEventsHandler(d *db.DB, n *notifications.Service, secret string) *BountyEventsHandler {
	return &BountyEventsHandler{db: d, notif: n, secret: secret}
}

type bountyEvent struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	GitHubUserID int64          `json:"githubUserId"`
	Payload      map[string]any `json:"payload"`
	OccurredAt   string         `json:"occurredAt"`
}

func (h *BountyEventsHandler) verify(raw []byte, header string) bool {
	if h.secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(raw)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}

// money renders an amount the way a person reads it, from minor units.
func money(payload map[string]any) string {
	minor, _ := payload["amount_minor"].(string)
	currency, _ := payload["currency"].(string)
	if minor == "" {
		return ""
	}
	// Six decimals for both USDC and ANSEM. Rendered rather than left in
	// minor units, because "1000000 USDC" in an email is alarming.
	whole, frac := minor, ""
	if len(minor) > 6 {
		whole, frac = minor[:len(minor)-6], strings.TrimRight(minor[len(minor)-6:], "0")
	} else {
		whole, frac = "0", strings.TrimRight(strings.Repeat("0", 6-len(minor))+minor, "0")
	}
	if frac == "" {
		return fmt.Sprintf("%s %s", whole, currency)
	}
	return fmt.Sprintf("%s.%s %s", whole, frac, currency)
}

// when renders a timestamp the way a person reads one.
//
// Every date a contributor sees uses this. Two formats for the same instant -
// an ISO string in one message and a written date in the next - makes somebody
// check whether they are even the same deadline, which is a cost paid by the
// person least able to afford the doubt.
func when(payload map[string]any, key string) string {
	raw, _ := payload[key].(string)
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// Unparseable is worth showing as-is rather than dropping: a wrong-
		// looking date is a bug report, a missing one is silence.
		return raw
	}
	return t.UTC().Format("2 January 2006 at 15:04 UTC")
}

func where(payload map[string]any) string {
	repo, _ := payload["repo"].(string)
	issue, ok := payload["issue_number"].(float64)
	if repo == "" {
		return ""
	}
	if !ok {
		return repo
	}
	return fmt.Sprintf("%s #%d", repo, int(issue))
}

// Receive handles POST /internal/bounty-events.
//
// Always answers 200 for an event it understood, even when nothing was sent -
// a user with the notification switched off, or one who has unlinked GitHub,
// is a legitimate outcome and not a delivery failure. Answering otherwise
// would make the agent retry forever over somebody's preference.
func (h *BountyEventsHandler) Receive(c *fiber.Ctx) error {
	raw := c.Body()
	if !h.verify(raw, c.Get("X-Bounty-Signature-256")) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "bad_signature"})
	}
	var e bountyEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "malformed"})
	}

	amount := money(e.Payload)
	place := where(e.Payload)

	var t notifications.Type
	var title, body string
	link := notifications.BountiesLink()

	switch e.Kind {
	case "bounty_draw_won":
		t = notifications.TypeBountyDrawWon
		title = "You won the draw"
		deadline := when(e.Payload, "staleAt")
		body = fmt.Sprintf(
			"The %s bounty on %s is yours. Open a pull request that says \"Closes #%v\" before %s. "+
				"If nothing arrives by then the bounty is drawn again and it counts as an abandon, which lowers your odds next time.",
			amount, place, e.Payload["issue_number"], deadline)
	case "bounty_assignment_expiring":
		t = notifications.TypeBountyAssignmentExpiring
		hours, _ := e.Payload["hoursLeft"].(float64)
		title = fmt.Sprintf("Your bounty assignment expires in %d hours", int(hours))
		body = fmt.Sprintf(
			"You hold the %s bounty on %s and no pull request has arrived yet. "+
				"If it lapses the bounty is drawn again and it counts as an abandon, which lowers your odds on future bounties.",
			amount, place)
		link = notifications.BountyRulesLink()
	case "bounty_paid":
		t = notifications.TypeBountyPaid
		title = "You've been paid"
		tx, _ := e.Payload["txUrl"].(string)
		body = fmt.Sprintf("%s for %s has been sent to your wallet. Transaction: %s", amount, place, tx)
	case "bounty_application_received":
		t = notifications.TypeBountyApplicationReceived
		title = "Application received"
		closes := when(e.Payload, "closesAt")
		body = fmt.Sprintf("You are in the draw for the bounty on %s. Applications close %s, and the draw runs after that.", place, closes)
	case "bounty_draw_lost":
		t = notifications.TypeBountyDrawLost
		title = "The draw went to someone else"
		body = fmt.Sprintf("The bounty on %s was drawn and went to another applicant. Applying again costs you nothing and your odds are unaffected.", place)
	case "bounty_unassigned":
		t = notifications.TypeBountyUnassigned
		title = "Your bounty assignment has ended"
		reason, _ := e.Payload["reason"].(string)
		actor, _ := e.Payload["actor"].(string)
		who := actor
		if who == "" {
			who = "A maintainer"
		}
		// The reason comes first because it is the only part that answers the
		// question somebody actually has. Then, plainly, that this is not held
		// against them - "unassigned" reads as a judgement unless it says
		// otherwise, and here it is not one.
		body = fmt.Sprintf(
			"%s ended your assignment on the %s bounty for %s. They gave this reason: %q\n\n"+
				"Nothing is counted against you. No abandon is recorded, your odds in future draws are unchanged, "+
				"and your application is back in the pool. The very next draw on this bounty skips you, so you are not "+
				"unassigned and reassigned in the same minute; after that you are eligible again, this bounty included.",
			who, amount, place, reason)
		link = notifications.BountiesLink()
	case "bounty_deadline_changed":
		t = notifications.TypeBountyDeadlineChanged
		title = "The deadline on your bounty has moved"
		reason, _ := e.Payload["reason"].(string)
		actor, _ := e.Payload["actor"].(string)
		previous := when(e.Payload, "previousAt")
		nowAt := when(e.Payload, "staleAt")
		who := actor
		if who == "" {
			who = "A maintainer"
		}
		body = fmt.Sprintf(
			"%s changed the deadline for opening a pull request on the %s bounty for %s. "+
				"It was %s and it is now %s. They gave this reason: %q\n\n"+
				"If no pull request arrives by the new deadline the bounty is drawn again, and that does count as an abandon.",
			who, amount, place, previous, nowAt, reason)
		link = notifications.BountiesLink()
	case "bounty_review_posted":
		t = notifications.TypeBountyReviewPosted
		title = "The agent reviewed your pull request"
		body = fmt.Sprintf("An advisory review is on your pull request for %s. A maintainer decides whether to merge; the review does not.", place)
	default:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown_kind", "kind": e.Kind})
	}

	// The recipient is resolved AFTER the kind is understood, and the order
	// matters. The other way round, an event whose kind we do not recognise -
	// a typo, or one added to the agent before this service knew about it -
	// was answered 200 whenever the person also had no account. The agent
	// marked it delivered and the event was gone. Two unrelated unknowns
	// cancelling out into a success is exactly the shape of a bug nobody
	// finds; a test passed locally only because some other test had left a
	// user with that GitHub id lying around.
	var userID uuid.UUID
	if err := h.db.Pool.QueryRow(c.Context(),
		`SELECT user_id FROM github_accounts WHERE github_user_id = $1`, e.GitHubUserID).Scan(&userID); err != nil {
		// This one IS an accepted outcome: no Grainlify account, or GitHub
		// unlinked. Not something the agent can fix by retrying.
		slog.Info("bounty event: no Grainlify user for that GitHub account", "github_user_id", e.GitHubUserID, "kind", e.Kind)
		return c.JSON(fiber.Map{"accepted": true, "notified": false, "reason": "no_linked_account"})
	}

	h.notif.Notify(c.Context(), userID, t, title, body, link)
	return c.JSON(fiber.Map{"accepted": true, "notified": true})
}
