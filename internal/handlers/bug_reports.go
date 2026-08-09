package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

const (
	maxBugReportDescriptionRunes = 2000

	// maxBugReportScreenshotBytes bounds the DECODED image size (after
	// stripping the data-URL base64 wrapper) - matches the frontend's own
	// upload cap (RewardsTab.tsx's MAX_SCREENSHOT_BYTES) so a rejection here
	// never surprises a user who already passed client-side validation.
	maxBugReportScreenshotBytes = 5 * 1024 * 1024
)

var allowedBugReportImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

type BugReportsHandler struct {
	cfg        config.Config
	httpClient *http.Client
}

func NewBugReportsHandler(cfg config.Config) *BugReportsHandler {
	return &BugReportsHandler{cfg: cfg, httpClient: &http.Client{Timeout: 10 * time.Second}}
}

type bugReportRequest struct {
	Description   string `json:"description"`
	Screenshot    string `json:"screenshot"`
	PageURL       string `json:"page_url"`
	ReporterLogin string `json:"reporter_login"`
}

// Create relays a bug report straight to the configured Discord webhook.
// Reports are not persisted anywhere in this database - the Discord channel
// is the system of record, matching the delivery mechanism chosen over a
// DB-backed admin view.
func (h *BugReportsHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if strings.TrimSpace(h.cfg.DiscordBugReportWebhookURL) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error":   "bug_reports_not_configured",
				"message": "DISCORD_BUG_REPORT_WEBHOOK_URL must be set",
			})
		}

		var req bugReportRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		description := strings.TrimSpace(req.Description)
		if description == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "description_required"})
		}
		if utf8.RuneCountInString(description) > maxBugReportDescriptionRunes {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "description_too_long"})
		}

		var imageBytes []byte
		var imageExt string
		if screenshot := strings.TrimSpace(req.Screenshot); screenshot != "" {
			var err error
			imageBytes, imageExt, err = decodeBugReportScreenshot(screenshot)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			}
		}

		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		if err := h.postToDiscord(ctx, bugReportContext{
			Description:   description,
			PageURL:       strings.TrimSpace(req.PageURL),
			ReporterLogin: strings.TrimSpace(req.ReporterLogin),
			UserAgent:     c.Get("User-Agent"),
			IP:            c.IP(),
			ImageBytes:    imageBytes,
			ImageExt:      imageExt,
		}); err != nil {
			slog.Error("bug_reports: discord relay failed", "error", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "discord_relay_failed"})
		}

		return c.JSON(fiber.Map{"ok": true})
	}
}

// decodeBugReportScreenshot parses a "data:image/<type>;base64,<payload>"
// data URL into real bytes plus a bare file extension. Deliberately stricter
// than social_follow.go's Submit(), which only checks the "data:image/"
// string prefix and stores the raw string as-is - this endpoint needs actual
// decoded bytes to attach as a Discord file, not just a string to persist.
func decodeBugReportScreenshot(dataURL string) ([]byte, string, error) {
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
	ext, ok := allowedBugReportImageTypes[mimeType]
	if !ok {
		return nil, "", fmt.Errorf("screenshot_must_be_an_image")
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("screenshot_invalid")
	}
	if len(decoded) > maxBugReportScreenshotBytes {
		return nil, "", fmt.Errorf("screenshot_too_large")
	}
	return decoded, ext, nil
}

type bugReportContext struct {
	Description   string
	PageURL       string
	ReporterLogin string
	UserAgent     string
	IP            string
	ImageBytes    []byte
	ImageExt      string
}

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

// postToDiscord builds a multipart/form-data request matching Discord's
// webhook API: a payload_json field carrying the embed, plus an optional
// files[0] part for the screenshot, with the embed's image.url referencing
// attachment://<filename> so Discord renders it inline rather than as a bare
// attachment. This is the first multipart-outbound call in this codebase -
// client construction/timeout/error-wrap style otherwise matches
// internal/email/mailercloud.go.
func (h *BugReportsHandler) postToDiscord(ctx context.Context, br bugReportContext) error {
	embed := discordEmbed{
		Title:       "🐛 New bug report",
		Description: truncateRunes(br.Description, 4096),
		Color:       0xc9983a,
		Fields: []discordEmbedField{
			{Name: "Page", Value: valueOrDefault(br.PageURL, "unknown"), Inline: false},
			{Name: "Reporter", Value: valueOrDefault(br.ReporterLogin, "anonymous"), Inline: true},
			{Name: "IP", Value: valueOrDefault(br.IP, "unknown"), Inline: true},
			{Name: "User agent", Value: truncateRunes(valueOrDefault(br.UserAgent, "unknown"), 1024), Inline: false},
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	fileName := ""
	if len(br.ImageBytes) > 0 {
		fileName = "screenshot." + br.ImageExt
		embed.Image = &discordEmbedImage{URL: "attachment://" + fileName}
	}

	payloadJSON, err := json.Marshal(discordWebhookPayload{Embeds: []discordEmbed{embed}})
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}
	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return fmt.Errorf("write payload_json field: %w", err)
	}

	if fileName != "" {
		part, err := writer.CreateFormFile("files[0]", fileName)
		if err != nil {
			return fmt.Errorf("create form file: %w", err)
		}
		if _, err := part.Write(br.ImageBytes); err != nil {
			return fmt.Errorf("write screenshot bytes: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.DiscordBugReportWebhookURL, body)
	if err != nil {
		return fmt.Errorf("build discord request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("discord webhook returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
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
