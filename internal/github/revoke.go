package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// applicationsAPIBase is a var so tests can point it at an httptest.Server.
var applicationsAPIBase = "https://api.github.com/applications"

// ErrGrantNotFound means GitHub has no grant for this token: the person
// already revoked Grainlify at github.com/settings/applications, or the token
// was already invalid. For an erasure that is the outcome wanted.
var ErrGrantNotFound = errors.New("github: grant not found")

// RevokeGrant deletes Grainlify's authorization for the person who owns
// accessToken, not just that one token.
//
// https://docs.github.com/en/rest/apps/oauth-applications (read 2026-10-03):
// "Deleting an application's grant will also delete all OAuth tokens
// associated with the application for the user. Once deleted, the
// application will have no access to the user's account and will no longer
// be listed on the application authorizations settings screen within GitHub."
//
// The grant rather than the token because erasing an account should leave
// nothing behind on GitHub's side either: revoking one token would leave the
// authorization listed, and any other token minted for it valid. Basic auth
// with the app's client id and secret, as the endpoint requires.
func RevokeGrant(ctx context.Context, clientID, clientSecret, accessToken string) error {
	if clientID == "" || clientSecret == "" {
		return errors.New("github: oauth app not configured")
	}
	if accessToken == "" {
		return errors.New("github: empty access token")
	}
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	u := fmt.Sprintf("%s/%s/grant", applicationsAPIBase, url.PathEscape(clientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github: build revoke request: %w", err)
	}
	req.SetBasicAuth(clientID, clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: revoke grant: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64*1024))

	switch {
	case res.StatusCode == http.StatusNoContent || (res.StatusCode >= 200 && res.StatusCode < 300):
		return nil
	case res.StatusCode == http.StatusNotFound:
		return ErrGrantNotFound
	default:
		return fmt.Errorf("github: revoke grant: status %d", res.StatusCode)
	}
}
