package handlers

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Support requests: persisted first, delivered second.
//
// The endpoint this replaces relayed straight to a Discord webhook and stored
// nothing. Its own comment said "the Discord channel is the system of record",
// which meant a failed webhook returned 502 and the report was gone - along
// with whatever the person had typed. That is the one thing a support system
// must not do.
//
// The order is now: validate, write the row, then fan out. Delivery failures
// are recorded per sink and do not fail the request, because the report is
// already safe. The only path that can still lose a report is the database
// write itself, and that is an honest 5xx: the caller is told it was not
// saved, so they can retry, rather than being shown a success for something
// that does not exist.

const (
	maxSupportMessageRunes = 2000

	// Bounds the DECODED image (after stripping the data-URL wrapper).
	//
	// Cut from 5MB to 2MB. 5 matched the frontend's own upload cap, which was
	// the right instinct when the only way here was a widget inside the app;
	// /support is now a public route reachable by anyone, and the per-request
	// amplification is what an anonymous endpoint costs when it is hammered.
	// 2MB is still generous for a screenshot - the largest we have received is
	// well under it - and there is no judgement in the change: it reduces the
	// worst case by 60% and rejects nothing anybody has actually sent.
	//
	// The frontend cap is now the LOWER of the two by design: a rejection here
	// should be unreachable in normal use, and reachable only by something not
	// using our form.
	maxSupportScreenshotBytes = 2 * 1024 * 1024

	// A global ceiling on anonymous submissions per hour, across everybody.
	//
	// Deliberately not per-client. The per-client limiter that already guards
	// this route keys on Fiber's c.IP(), which behind Railway is the edge
	// proxy's address and not the caller's - every one of the ten support
	// reports we have ever received recorded a 100.64.0.x CGNAT address, six
	// distinct ones, and 14 rapid requests from one machine never triggered
	// the 10/minute limit. Until the proxy's forwarding behaviour is
	// established there is no trustworthy key, so this uses none.
	//
	// Crude on purpose, and honest about the trade: under a flood this blocks
	// legitimate reporters too. That is the lesser harm. What it bounds is
	// persistence and the Telegram/Discord fan-out, which is what actually
	// costs something - a refused submission costs a person one retry, an
	// unbounded one costs the channel everybody else reads.
	//
	// 60/hour against 10 reports in the platform's entire history is invisible
	// in normal use and still a hard stop. Anonymous only: a signed-in report
	// is attributable, so it is bounded by the account rather than by this.
	maxAnonymousSupportRequestsPerHour = 60

	// How long the whole fan-out may take. Deliberately shorter than the sum of
	// the sinks' own timeouts: the row is already durable, so there is nothing
	// to gain by making somebody wait for a sink that is clearly unwell.
	supportFanOutTimeout = 8 * time.Second
)

var allowedSupportImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// The categories the widget offers. Mirrors the CHECK constraint on
// support_requests.category - a value not in this set is rejected at the edge
// rather than by a constraint violation five lines later.
var supportCategories = map[string]bool{
	"bug": true, "kyc": true, "idea": true, "help": true, "other": true,
}

type SupportRequestsHandler struct {
	cfg   config.Config
	db    *db.DB
	sinks []SupportSink
}

func NewSupportRequestsHandler(cfg config.Config, d *db.DB) *SupportRequestsHandler {
	return &SupportRequestsHandler{
		cfg: cfg,
		db:  d,
		// Peers. Each is attempted independently; one being unconfigured or
		// down must not affect the other, and neither can fail the request.
		sinks: []SupportSink{
			newDiscordSupportSink(cfg.DiscordBugReportWebhookURL),
			newTelegramSupportSink(telegramSinkConfigFrom(cfg)),
		},
	}
}

type supportRequestBody struct {
	// Category is optional for backward compatibility: the widget currently
	// posts bug reports without one, and defaults to "bug".
	Category string `json:"category"`
	Message  string `json:"message"`
	// Description is the old field name, still accepted so a cached frontend
	// bundle keeps working through the deploy.
	Description string `json:"description"`
	Screenshot  string `json:"screenshot"`
	PageURL     string `json:"page_url"`
}

// Create handles POST /support-requests (and the legacy /bug-reports path).
func (h *SupportRequestsHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		var body supportRequestBody
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		message := strings.TrimSpace(body.Message)
		if message == "" {
			message = strings.TrimSpace(body.Description)
		}
		if message == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "message_required"})
		}
		if utf8.RuneCountInString(message) > maxSupportMessageRunes {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "message_too_long"})
		}

		category := strings.ToLower(strings.TrimSpace(body.Category))
		if category == "" {
			category = "bug"
		}
		if !supportCategories[category] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_category"})
		}

		var imageBytes []byte
		var imageExt string
		screenshot := strings.TrimSpace(body.Screenshot)
		if screenshot != "" {
			var err error
			imageBytes, imageExt, err = decodeSupportScreenshot(screenshot)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			}
		}

		// Identity comes from the token, never from the body.
		//
		// This endpoint is deliberately unauthenticated - somebody who cannot
		// sign in is exactly the person most likely to need support - so a
		// missing or invalid token means anonymous, not rejected. What it must
		// never mean is "believe the browser": the previous version took a
		// `reporter_login` field from the request body and relayed it as the
		// reporter's identity, so anyone could file a report as anyone.
		userID, reporterLogin := h.identify(c)

		// The global ceiling on ANONYMOUS submissions, checked after identity
		// is known and before anything is written.
		//
		// Counted from the table rather than held in memory, so it survives a
		// restart and is not per-instance - an in-process counter would reset
		// on every deploy and multiply by the replica count, which is the
		// difference between a bound and a suggestion.
		//
		// A signed-in report is exempt: it is attributable, so the account
		// bounds it. That also means the failure mode under a flood is
		// "anonymous reporting pauses", not "support stops", and anybody with
		// an account can still get through.
		if userID == nil {
			var recent int
			if err := h.db.Pool.QueryRow(c.Context(), `
SELECT count(*)::int FROM support_requests
WHERE user_id IS NULL AND created_at > now() - interval '1 hour'
`).Scan(&recent); err != nil {
				// Fail OPEN, deliberately. A counting query that errors must
				// not become an outage on the one route somebody locked out of
				// their account can reach. Logged loudly so it cannot be the
				// silent removal of a bound.
				slog.Error("support: anonymous cap check failed, allowing the request", "error", err)
			} else if recent >= maxAnonymousSupportRequestsPerHour {
				slog.Warn("support: anonymous hourly cap reached, refusing",
					"recent", recent, "cap", maxAnonymousSupportRequestsPerHour)
				return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
					"error": "rate_limited",
					// Says what to do rather than only that it failed. Somebody
					// hitting this is far more likely to be a real person caught
					// behind a flood than the flood itself.
					"message": "We're getting an unusual number of reports right now. " +
						"Please try again shortly — or sign in, which isn't affected.",
				})
			}
		}

		id := uuid.New()
		var screenshotStored *string
		if screenshot != "" {
			screenshotStored = &screenshot
		}
		var pageURL, userAgent, ip *string
		if v := strings.TrimSpace(body.PageURL); v != "" {
			pageURL = &v
		}
		if v := strings.TrimSpace(c.Get("User-Agent")); v != "" {
			userAgent = &v
		}
		if v := strings.TrimSpace(c.IP()); v != "" {
			ip = &v
		}

		// DIAGNOSTIC, and temporary. Remove once the proxy question is settled.
		//
		// c.IP() has never once recorded a real caller: every reporter_ip in
		// the table is a 100.64.0.x Railway CGNAT address, six distinct ones
		// across ten reports. That is why the per-client limiter on this route
		// has never fired - it keys on an address that rotates per request.
		//
		// Fixing it means setting fiber.Config.ProxyHeader, and which value to
		// trust depends entirely on what the edge does with an INBOUND
		// X-Forwarded-For. If it strips client-supplied copies, the leftmost
		// entry is the caller. If it appends without stripping, the leftmost is
		// whatever the caller made up and only the rightmost is real. Public
		// answers contradict each other on exactly this point, so this logs
		// what our own edge actually sends and the question gets settled with
		// evidence rather than a citation.
		//
		// Logs header values, not bodies, on a route that already records an
		// address. It changes no behaviour: c.IP() is untouched.
		slog.Info("support: forwarding headers observed",
			"remote_ip", c.IP(),
			"x_forwarded_for", c.Get("X-Forwarded-For"),
			"x_real_ip", c.Get("X-Real-IP"),
			"x_envoy_external_address", c.Get("X-Envoy-External-Address"),
			"forwarded", c.Get("Forwarded"))

		// PERSIST FIRST. Everything after this point is best-effort.
		if _, err := h.db.Pool.Exec(c.Context(), `
INSERT INTO support_requests
  (id, user_id, category, message, page_url, user_agent, screenshot_url, reporter_ip)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`, id, userID, category, message, pageURL, userAgent, screenshotStored, ip); err != nil {
			// The only remaining path where a report can be lost. Say so: a
			// success here would tell somebody their problem had been reported
			// when nothing exists, and they would wait for a reply that is
			// never coming. The frontend keeps their text so they can retry.
			slog.Error("support_requests: insert failed - report NOT saved",
				"error", err, "category", category, "request_id", c.Locals("requestid"))
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error":   "report_not_saved",
				"message": "We couldn't save your report, so it hasn't been sent. Your message is still here - please try again.",
			})
		}

		req := SupportRequest{
			ID: id, Category: category, Message: message,
			PageURL: strings.TrimSpace(body.PageURL), UserAgent: strings.TrimSpace(c.Get("User-Agent")),
			UserID: userID, ReporterLogin: reporterLogin,
			ScreenshotBytes: imageBytes, ScreenshotExt: imageExt,
		}
		delivered := h.fanOut(c.Context(), req)

		// 200 regardless of delivery. The report exists; a sink being down is
		// an operational problem, not the reporter's problem.
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":         true,
			"support_id": id.String(),
			"delivered":  delivered,
		})
	}
}

// identify resolves the caller from the Authorization header if one is present
// and valid. Anything else is anonymous - no error, no rejection.
func (h *SupportRequestsHandler) identify(c *fiber.Ctx) (*uuid.UUID, string) {
	header := strings.TrimSpace(c.Get("Authorization"))
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return nil, ""
	}
	token := strings.TrimSpace(header[len("bearer "):])
	if token == "" {
		return nil, ""
	}
	claims, err := auth.ParseJWT(h.cfg.JWTSecret, token)
	if err != nil {
		// A bad token is anonymous, not a 401. Someone with an expired session
		// reporting that their session expired is a real case.
		return nil, ""
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return nil, ""
	}

	// Login is looked up here rather than trusted from anywhere client-side.
	var login *string
	_ = h.db.Pool.QueryRow(c.Context(),
		`SELECT login FROM github_accounts WHERE user_id = $1`, id).Scan(&login)
	if login == nil {
		return &id, ""
	}
	return &id, *login
}

// fanOut attempts every configured sink and records the ones that succeeded.
// Returns the names that took the report, for the response.
//
// Sinks are independent: one failing must not skip another, and a failure is
// recorded as an absent timestamp rather than propagated to the caller.
func (h *SupportRequestsHandler) fanOut(ctx context.Context, req SupportRequest) []string {
	fanCtx, cancel := context.WithTimeout(ctx, supportFanOutTimeout)
	defer cancel()

	delivered := []string{}
	for _, sink := range h.sinks {
		if !sink.Configured() {
			// Not an error. A sink that has not been set up is not an outage.
			slog.Debug("support_requests: sink not configured, skipping",
				"sink", sink.Name(), "support_id", req.ID)
			continue
		}
		if !sink.Handles(req.Category) {
			// Also not an error, and deliberately not a delivery: the column
			// stays NULL for ever and supportDelivered knows that. This is how
			// a KYC request stays out of Discord - by code, not by trusting a
			// channel permission to remain correct.
			slog.Debug("support_requests: sink does not handle this category, skipping",
				"sink", sink.Name(), "category", req.Category, "support_id", req.ID)
			continue
		}
		result, err := sink.Deliver(fanCtx, req)
		if err != nil {
			slog.Error("support_requests: delivery failed",
				"sink", sink.Name(), "support_id", req.ID, "category", req.Category, "error", err)
			continue
		}
		if err := h.markDelivered(ctx, req.ID, sink.Name(), req.Category, result); err != nil {
			// Delivered but not recorded. Logged loudly because it makes a
			// replay look necessary when it is not - a duplicate is the cost.
			slog.Error("support_requests: delivered but failed to record it",
				"sink", sink.Name(), "support_id", req.ID, "error", err)
		}
		delivered = append(delivered, sink.Name())
	}
	return delivered
}

// markDelivered stamps the column that this sink, for this category, actually
// delivered to.
//
// The column is chosen from a fixed switch rather than interpolated from
// sink.Name(), so a sink cannot inject SQL by choosing its own name. The
// category branch inside "telegram" is the delivery rule from
// support_delivery.go expressed as a write: KYC delivers to the admin DM and
// nowhere else, everything else to its topic, and the two must never share a
// column or a posted stub becomes indistinguishable from delivered details.
func (h *SupportRequestsHandler) markDelivered(
	ctx context.Context, id uuid.UUID, sink, category string, result SupportDeliveryResult,
) error {
	switch sink {
	case "discord":
		_, err := h.db.Pool.Exec(ctx,
			`UPDATE support_requests SET discord_delivered_at = now() WHERE id = $1`, id)
		return err
	case "telegram":
		if category == "kyc" {
			// No public post exists for KYC, so this column is the whole
			// delivery. Nothing can route to a fallback here either - a DM has
			// no topic.
			_, err := h.db.Pool.Exec(ctx,
				`UPDATE support_requests SET telegram_admin_dm_delivered_at = now() WHERE id = $1`, id)
			return err
		}
		_, err := h.db.Pool.Exec(ctx, `
UPDATE support_requests
SET telegram_delivered_at = now(),
    telegram_routed_to_fallback = $2
WHERE id = $1`, id, result.RoutedToFallback)
		return err
	default:
		return fmt.Errorf("no delivery column for sink %q", sink)
	}
}

// decodeSupportScreenshot parses a "data:image/<type>;base64,<payload>" data
// URL into bytes plus a bare extension.
func decodeSupportScreenshot(dataURL string) ([]byte, string, error) {
	const prefix = "data:"
	if !strings.HasPrefix(dataURL, prefix) {
		return nil, "", fmt.Errorf("screenshot_must_be_an_image")
	}
	commaIdx := strings.IndexByte(dataURL, ',')
	if commaIdx == -1 {
		return nil, "", fmt.Errorf("screenshot_invalid")
	}
	header := dataURL[len(prefix):commaIdx]
	payload := dataURL[commaIdx+1:]

	headerParts := strings.Split(header, ";")
	mimeType := headerParts[0]
	isBase64 := false
	for _, p := range headerParts[1:] {
		if p == "base64" {
			isBase64 = true
		}
	}
	if !isBase64 {
		return nil, "", fmt.Errorf("screenshot_invalid")
	}
	ext, ok := allowedSupportImageTypes[mimeType]
	if !ok {
		return nil, "", fmt.Errorf("screenshot_must_be_an_image")
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("screenshot_invalid")
	}
	if len(decoded) > maxSupportScreenshotBytes {
		return nil, "", fmt.Errorf("screenshot_too_large")
	}
	return decoded, ext, nil
}

// Discord embed shapes, kept here because the Discord sink is the only user.
type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbedImage struct {
	URL string `json:"url"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields"`
	Image       *discordEmbedImage  `json:"image,omitempty"`
	Timestamp   string              `json:"timestamp"`
}

type discordWebhookPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

func valueOrDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
