package events

import (
	"encoding/json"
	"errors"
)

const (
	SubjectGitHubWebhookReceived = "github.webhook.received"
)

var (
	ErrMissingDeliveryID = errors.New("github webhook received event missing delivery_id")
	ErrMissingEvent      = errors.New("github webhook received event missing event")
	ErrMissingPayload    = errors.New("github webhook received event missing payload")
)

type GitHubWebhookReceived struct {
	DeliveryID   string          `json:"delivery_id"`
	Event        string          `json:"event"`
	Action       string          `json:"action,omitempty"`
	RepoFullName string          `json:"repo_full_name,omitempty"`
	Payload      json.RawMessage `json:"payload"`
}

func (e GitHubWebhookReceived) Validate() error {
	var errs []error
	if e.DeliveryID == "" {
		errs = append(errs, ErrMissingDeliveryID)
	}
	if e.Event == "" {
		errs = append(errs, ErrMissingEvent)
	}
	if len(e.Payload) == 0 {
		errs = append(errs, ErrMissingPayload)
	}
	return errors.Join(errs...)
}
