package didit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// deleteBaseURL is Didit's v3 API, where session deletion lives; the rest of
// this client is on v2, which has no delete. A var so a test can point it at
// an httptest.Server.
//
// https://docs.didit.me/sessions-api/delete-session (read 2026-10-03):
// DELETE /v3/session/{session_id}/delete/ removes "the decision, the
// extracted data, the associated feature records ... and all stored media
// (document images, videos, portraits, selfies ...)". It needs the API key to
// hold the delete:sessions privilege.
var deleteBaseURL = "https://verification.didit.me/v3"

// ErrSessionNotFound means Didit has no such session: it was deleted already,
// or never existed under this key. For an erasure that is the outcome wanted,
// so callers treat it as done rather than as a failure.
var ErrSessionNotFound = errors.New("didit: session not found")

// DeleteSession asks Didit to delete a verification session and everything
// it holds for it. Deletion cannot be undone.
//
// Didit's documentation is explicit about what this does NOT remove: webhook
// deliveries already queued, blocklist entries made from the session, and the
// parent User entity (a separate endpoint). Grainlify creates no Didit User
// entities and no blocklist entries, so the session is everything there is.
//
// The response body is never included in an error. It is Didit's answer about
// a person's identity session, and errors end up in logs.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	if c.APIKey == "" {
		return errors.New("didit: no API key configured")
	}
	if sessionID == "" {
		return errors.New("didit: empty session id")
	}
	u := fmt.Sprintf("%s/session/%s/delete/", deleteBaseURL, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("didit: build delete request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("didit: delete session: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64*1024))

	switch {
	case res.StatusCode == http.StatusNotFound:
		return ErrSessionNotFound
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	default:
		// 401/403 here most likely means the key lacks delete:sessions, which
		// is a console setting, not a code fix - said so the log line is
		// actionable.
		return fmt.Errorf("didit: delete session: status %d (401/403 usually means the API key lacks the delete:sessions privilege)", res.StatusCode)
	}
}
