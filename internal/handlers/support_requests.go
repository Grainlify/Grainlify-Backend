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

	// Bounds the DECODED image (after stripping the data-URL wrapper), matching
	// the frontend's own upload cap so a rejection here never surprises someone
	// who already passed client-side validation.
	maxSupportScreenshotBytes = 5 * 1024 * 1024

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
			newTelegramSupportSink(telegramSinkConfig{
				BotToken:    cfg.TelegramBotToken,
				ChatID:      cfg.TelegramChatID,
				AdminUserID: cfg.TelegramAdminUserID,
				TopicBugs:   cfg.TelegramTopicBugs,
				TopicKYC:    cfg.TelegramTopicKYC,
				TopicIdeas:  cfg.TelegramTopicIdeas,
				TopicHelp:   cfg.TelegramTopicHelp,
				TopicOther:  cfg.TelegramTopicOther,
			}),
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
