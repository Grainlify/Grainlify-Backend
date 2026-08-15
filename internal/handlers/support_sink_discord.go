package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Discord sink. Same wire format as before - a payload_json embed plus an
// optional files[0] part, with the embed's image.url referencing
// attachment://<filename> so the screenshot renders inline.
//
// Two things changed when this moved behind SupportSink:
//
//   - The reporter's IP is gone from the payload. It was a field on the embed;
//     it is now recorded on the row and sent nowhere.
//   - The reporter login is resolved server-side rather than taken from the
//     request body, so it can no longer be spoofed by whoever is posting.
type discordSupportSink struct {
	webhookURL string
	httpClient *http.Client
}

func newDiscordSupportSink(webhookURL string) *discordSupportSink {
	return &discordSupportSink{
		webhookURL: strings.TrimSpace(webhookURL),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *discordSupportSink) Name() string     { return "discord" }
func (s *discordSupportSink) Configured() bool { return s.webhookURL != "" }

var supportCategoryTitles = map[string]string{
	"bug":   "🐛 Bug report",
	"kyc":   "🪪 KYC / verification",
	"idea":  "💡 Improvement",
	"help":  "🙋 Help request",
	"other": "💬 Support request",
}

func (s *discordSupportSink) Deliver(ctx context.Context, r SupportRequest) (SupportDeliveryResult, error) {
	title := supportCategoryTitles[r.Category]
	if title == "" {
		title = "💬 Support request"
	}

	embed := discordEmbed{
		Title:       title,
		Description: truncateRunes(r.Message, 4096),
		Color:       0xc9983a,
		Fields: []discordEmbedField{
			// The support ID is the link back to the full row, and is what a
			// reply to the reporter should quote.
			{Name: "Support ID", Value: r.ID.String(), Inline: false},
			{Name: "Page", Value: valueOrDefault(r.PageURL, "unknown"), Inline: false},
			{Name: "Reporter", Value: valueOrDefault(r.ReporterLogin, "anonymous"), Inline: true},
			{Name: "User agent", Value: truncateRunes(valueOrDefault(r.UserAgent, "unknown"), 1024), Inline: false},
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	fileName := ""
	if len(r.ScreenshotBytes) > 0 {
		fileName = "screenshot." + r.ScreenshotExt
		embed.Image = &discordEmbedImage{URL: "attachment://" + fileName}
	}

	payloadJSON, err := json.Marshal(discordWebhookPayload{Embeds: []discordEmbed{embed}})
	if err != nil {
		return SupportDeliveryResult{}, fmt.Errorf("marshal discord payload: %w", err)
	}
	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return SupportDeliveryResult{}, fmt.Errorf("write payload_json field: %w", err)
	}
	if fileName != "" {
		part, err := writer.CreateFormFile("files[0]", fileName)
		if err != nil {
			return SupportDeliveryResult{}, fmt.Errorf("create form file: %w", err)
		}
		if _, err := part.Write(r.ScreenshotBytes); err != nil {
			return SupportDeliveryResult{}, fmt.Errorf("write screenshot bytes: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return SupportDeliveryResult{}, fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, body)
	if err != nil {
		return SupportDeliveryResult{}, fmt.Errorf("build discord request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return SupportDeliveryResult{}, fmt.Errorf("discord webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return SupportDeliveryResult{}, fmt.Errorf("discord webhook returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return SupportDeliveryResult{}, nil
}
