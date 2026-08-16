package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
)

// Attaching a screenshot to a Telegram message.
//
// # Screenshots never reach the group
//
// @Grainlify is public and readable without joining, so anything posted to a
// topic is published. The topic text is already redacted down to the message,
// the page and the support id for exactly that reason - and then an image
// would undo all of it, because we cannot know what a screenshot contains
// before it posts. People capture whole windows: submissions have already
// arrived carrying an unrelated chat, and a billing page showing a name and an
// amount.
//
// A privacy property that depends on the reporter cropping carefully is not a
// property. So the image goes to the admin's direct message and the topic gets
// the text plus the support id, which is the link between the two. This is the
// same reasoning that took KYC out of Discord rather than relying on a channel
// being private.
//
// # Text first, image second
//
// Two reasons, and the first is not a preference:
//
//   - A caption is capped at 1024 characters. The topic text runs to about
//     3100 - the message alone is truncated at 3000. Sending the report as a
//     captioned photo would cost two thirds of it to gain an image, which is
//     the wrong way round: the text is the report.
//   - It makes the failure isolation structural rather than careful. By the
//     time anything can go wrong with the image, the report has already
//     arrived. A screenshot that cannot be sent costs the screenshot.
//
// # Telegram's documented limits for sendPhoto
//
//	photo at most 10 MB
//	width + height at most 10000 in total
//	width / height ratio at most 20
//
// Only the last two bind on us. Our own cap is 5 MB
// (maxSupportScreenshotBytes) so the size limit cannot be reached, but a
// full-page screenshot is easily 1512x9000 - a total of 10512, rejected at
// well under a megabyte. It is a shape problem rather than a size one.
//
// Oversized or wrongly-shaped images are sent with sendDocument instead, which
// allows 50 MB and imposes no dimensions. It arrives as a file rather than an
// inline preview - a downgrade in presentation, which is the point: the
// alternative is dropping it.

const (
	// telegramPhotoMaxBytes is Telegram's limit, not ours. Ours is lower.
	telegramPhotoMaxBytes = 10 * 1024 * 1024
	// telegramPhotoMaxDimensionSum is width + height.
	telegramPhotoMaxDimensionSum = 10000
	// telegramPhotoMaxRatio is the longer side over the shorter one.
	telegramPhotoMaxRatio = 20
)

// telegramPhotoRejection explains why an image cannot go as a photo, in words
// that belong in a log line. Empty means it can.
func telegramPhotoRejection(data []byte) string {
	if len(data) > telegramPhotoMaxBytes {
		return fmt.Sprintf("%d bytes exceeds Telegram's 10 MB photo limit", len(data))
	}
	// DecodeConfig reads the header only - it does not decode the pixels, so
	// this stays cheap on a large image.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Unreadable header is not proof the image is bad; Telegram may still
		// accept it. Send as a document rather than guess.
		return "dimensions could not be read: " + err.Error()
	}
	if cfg.Width+cfg.Height > telegramPhotoMaxDimensionSum {
		return fmt.Sprintf("%dx%d exceeds Telegram's limit of %d for width+height (full-page screenshots hit this)",
			cfg.Width, cfg.Height, telegramPhotoMaxDimensionSum)
	}
	long, short := cfg.Width, cfg.Height
	if short > long {
		long, short = short, long
	}
	if short == 0 || long/short > telegramPhotoMaxRatio {
		return fmt.Sprintf("%dx%d exceeds Telegram's %d:1 ratio limit", cfg.Width, cfg.Height, telegramPhotoMaxRatio)
	}
	return ""
}

// sendFile uploads one image with sendPhoto or sendDocument.
func (s *telegramSupportSink) sendFile(
	ctx context.Context, method, field, chatID, threadID string, replyTo int64, data []byte, filename, caption string,
) (*telegramResponse, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	_ = w.WriteField("chat_id", chatID)
	if threadID != "" {
		_ = w.WriteField("message_thread_id", threadID)
	}
	if replyTo != 0 {
		// Keeps the image attached to its report in a busy topic. Not required
		// for delivery, so a failure to resolve it must not matter - see
		// allow_sending_without_reply.
		_ = w.WriteField("reply_to_message_id", fmt.Sprintf("%d", replyTo))
		_ = w.WriteField("allow_sending_without_reply", "true")
	}
	if caption != "" {
		_ = w.WriteField("caption", truncateRunes(caption, 1024))
	}

	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("write screenshot bytes: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := s.baseURL + "/bot" + s.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Never include the endpoint in an error: it carries the bot token.
		return nil, fmt.Errorf("telegram %s failed: %w", method, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var parsed telegramResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("telegram returned unparsable body (status %d)", resp.StatusCode)
	}
	if !parsed.OK {
		return &parsed, fmt.Errorf("telegram error %d: %s", parsed.ErrorCode, parsed.Description)
	}
	return &parsed, nil
}

// attachScreenshot sends the image after its text message.
//
// Never returns an error, by design. Everything it can fail at happens after
// the report itself has been delivered, and the caller's delivery result must
// not change because an image did not make it. Failures are logged, and the
// topic gets a short note so a missing screenshot is visible rather than
// silent - somebody reading the report can then ask for it, or open the row.
func (s *telegramSupportSink) attachScreenshot(
	ctx context.Context, r SupportRequest, chatID, threadID string, replyTo int64,
) {
	if len(r.ScreenshotBytes) == 0 {
		return
	}
	ext := r.ScreenshotExt
	if ext == "" {
		ext = "png"
	}
	filename := "screenshot-" + r.ID.String() + "." + ext
	// The caption is a label, not the report - but it is the ONLY thing tying
	// this image to its report, because the two are now in different chats and
	// no reply link can span them. Support id first, so it matches the line at
	// the bottom of the topic post.
	reporter := r.ReporterLogin
	if reporter == "" {
		reporter = "anonymous"
	}
	caption := "📎 Screenshot for " + r.ID.String() +
		"\nCategory: " + valueOrDefault(r.Category, "unknown") +
		"\nFrom: " + reporter +
		"\nPage: " + valueOrDefault(r.PageURL, "unknown")

	method, field := "sendPhoto", "photo"
	if why := telegramPhotoRejection(r.ScreenshotBytes); why != "" {
		// Stated, not silent: this is the branch where presentation degrades,
		// and a downgrade nobody can see is indistinguishable from a bug.
		slog.Warn("telegram: screenshot cannot be sent as a photo, sending as a file instead",
			"support_id", r.ID, "reason", why, "bytes", len(r.ScreenshotBytes))
		method, field = "sendDocument", "document"
		caption += " (sent as a file: " + why + ")"
	}

	if _, err := s.sendFile(ctx, method, field, chatID, threadID, replyTo, r.ScreenshotBytes, filename, caption); err == nil {
		return
	} else if method == "sendPhoto" {
		// Our checks agreed with the documented limits and Telegram still said
		// no. Retry as a document rather than lose the image to a constraint
		// we did not know about.
		slog.Warn("telegram: sendPhoto refused an image that passed our checks, retrying as a file",
			"support_id", r.ID, "error", err)
		if _, derr := s.sendFile(ctx, "sendDocument", "document", chatID, threadID, replyTo,
			r.ScreenshotBytes, filename, caption); derr == nil {
			return
		}
	}

	slog.Error("telegram: SCREENSHOT NOT DELIVERED - the report arrived without its image",
		"support_id", r.ID, "bytes", len(r.ScreenshotBytes), "method", method)

	// Say so where the report is, so the gap is visible to whoever reads it.
	note := "⚠️ A screenshot was attached to " + r.ID.String() + " but could not be delivered here. It is stored on the support row."
	if _, err := s.sendMessage(ctx, chatID, threadID, note); err != nil {
		slog.Error("telegram: could not post the missing-screenshot note either",
			"support_id", r.ID, "error", err)
	}
}

// screenshotNoteForTopic tells a topic reader that an image exists without
// posting it. Saying so matters: otherwise a report that depended on its
// screenshot reads as incomplete, and somebody re-asks the reporter for
// something they already sent.
func screenshotNoteForTopic(r SupportRequest) string {
	if len(r.ScreenshotBytes) == 0 {
		return ""
	}
	return "\n\n📎 A screenshot was attached. It is not posted here - see the support row, or the admin DM."
}

// screenshotNoteForDM is the equivalent for the KYC path, where the image
// follows in the same chat.
func screenshotNoteForDM(r SupportRequest) string {
	if len(r.ScreenshotBytes) == 0 {
		return ""
	}
	return "\n\n📎 Screenshot attached below."
}

// deliverScreenshotPrivately sends a public category's image to the admin DM.
//
// Never returns an error: it runs after the report has already reached its
// topic, and an image that cannot be delivered must not change that outcome.
func (s *telegramSupportSink) deliverScreenshotPrivately(ctx context.Context, r SupportRequest) {
	if len(r.ScreenshotBytes) == 0 {
		return
	}
	if s.adminUserID == "" {
		// The report is safe in its topic and on the row; only the image is
		// lost. Loud anyway, because the alternative to knowing this is
		// discovering it when somebody asks where a screenshot went.
		slog.Error("telegram: screenshot cannot be delivered - TELEGRAM_ADMIN_USER_ID is unset",
			"support_id", r.ID, "category", r.Category,
			"note", "screenshots are never posted to the public group, so there is no fallback destination")
		return
	}
	s.attachScreenshot(ctx, r, s.adminUserID, "", 0)
}
