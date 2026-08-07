package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// mailerCloudAPIURL is a var (not a const) so tests can point it at an
// httptest.Server instead of the real Mailercloud API.
var mailerCloudAPIURL = "https://email-api.mailercloud.com/email"

// MailerCloudMailer sends email via Mailercloud
// (https://apidoc.mailercloud.com/api-reference/email/send-email).
type MailerCloudMailer struct {
	apiKey   string
	from     string
	fromName string
	http     *http.Client
}

// NewMailerCloudMailer returns nil if apiKey or from is empty, so callers
// can treat a nil *MailerCloudMailer as "email sending disabled" without a
// separate enabled/disabled flag.
func NewMailerCloudMailer(apiKey, from, fromName string) *MailerCloudMailer {
	if apiKey == "" || from == "" {
		return nil
	}
	return &MailerCloudMailer{
		apiKey:   apiKey,
		from:     from,
		fromName: fromName,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

type mailerCloudRecipient struct {
	Email string `json:"email"`
}

type mailerCloudRecipients struct {
	To []mailerCloudRecipient `json:"to"`
}

type mailerCloudEmail struct {
	From       string                 `json:"from"`
	FromName   string                 `json:"fromName,omitempty"`
	Subject    string                 `json:"subject"`
	HTML       string                 `json:"html"`
	Recipients mailerCloudRecipients `json:"recipients"`
}

type mailerCloudSendRequest struct {
	Version string           `json:"version"`
	Email   mailerCloudEmail `json:"email"`
}

type mailerCloudResponse struct {
	Status     string `json:"status"`
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
}

func (m *MailerCloudMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	reqBody, err := json.Marshal(mailerCloudSendRequest{
		Version: "1.0",
		Email: mailerCloudEmail{
			From:     m.from,
			FromName: m.fromName,
			Subject:  subject,
			HTML:     htmlBody,
			Recipients: mailerCloudRecipients{
				To: []mailerCloudRecipient{{Email: to}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal mailercloud request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mailerCloudAPIURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build mailercloud request: %w", err)
	}
	// Mailercloud expects the raw API key in Authorization, no "Bearer" prefix.
	req.Header.Set("Authorization", m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("mailercloud request failed: %w", err)
	}
	defer resp.Body.Close()

	var parsed mailerCloudResponse
	_ = json.NewDecoder(resp.Body).Decode(&parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Status == "ERROR" {
		return fmt.Errorf("mailercloud: status %d, response status %q, message %q", resp.StatusCode, parsed.Status, parsed.Message)
	}
	return nil
}

var _ Mailer = (*MailerCloudMailer)(nil)
