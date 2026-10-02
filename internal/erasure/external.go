package erasure

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// External is every service outside our database that holds something about
// the person. An interface so the executor's tests can drive each outcome
// without the network.
type External interface {
	// RevokeGitHub deletes Grainlify's authorization on GitHub.
	RevokeGitHub(ctx context.Context, accessToken string) error
	// DeleteDiditSession asks Didit to delete a verification session.
	DeleteDiditSession(ctx context.Context, sessionID string) error
	// EraseAtAgent asks the bounty agent to erase what it holds for this
	// GitHub account. ErrAgentInFlight means it refused because a bounty
	// assignment or payout is in progress.
	EraseAtAgent(ctx context.Context, githubUserID int64, login string) error
}

var (
	// ErrNotConfigured means the service is switched off in this environment,
	// so there is nothing there to erase. Recorded, not retried.
	ErrNotConfigured = errors.New("erasure: service not configured")
	// ErrAgentInFlight means the bounty agent has an assignment or payout in
	// progress for this person. Treated as a hold, like money in flight here.
	ErrAgentInFlight = errors.New("erasure: bounty assignment or payout in progress at the agent")
)

// Services is the production External.
type Services struct {
	GitHubClientID     string
	GitHubClientSecret string
	Didit              *didit.Client
	// AgentURL and AgentKey reach the bounty agent the same way the rest of
	// the backend does: a message signed with BOUNTY_LINK_SIGNING_KEY, under
	// a domain of its own.
	AgentURL  string
	AgentKey  ed25519.PrivateKey
	AgentHTTP *http.Client
	Now       func() time.Time
}

// NewServices builds the production External from configuration values.
// Anything missing leaves that one step reporting ErrNotConfigured.
func NewServices(ghClientID, ghClientSecret, diditAPIKey, agentURL, agentKeyB64 string) *Services {
	s := &Services{
		GitHubClientID:     ghClientID,
		GitHubClientSecret: ghClientSecret,
		AgentURL:           strings.TrimRight(agentURL, "/"),
		AgentHTTP:          &http.Client{Timeout: 20 * time.Second},
		Now:                time.Now,
	}
	if diditAPIKey != "" {
		s.Didit = didit.NewClient(diditAPIKey)
	}
	if seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(agentKeyB64)); err == nil && len(seed) == ed25519.SeedSize {
		s.AgentKey = ed25519.NewKeyFromSeed(seed)
	}
	return s
}

func (s *Services) RevokeGitHub(ctx context.Context, accessToken string) error {
	if s.GitHubClientID == "" || s.GitHubClientSecret == "" {
		return ErrNotConfigured
	}
	return github.RevokeGrant(ctx, s.GitHubClientID, s.GitHubClientSecret, accessToken)
}

func (s *Services) DeleteDiditSession(ctx context.Context, sessionID string) error {
	if s.Didit == nil {
		return ErrNotConfigured
	}
	return s.Didit.DeleteSession(ctx, sessionID)
}

// AgentErasureDomain prefixes what the backend signs for an erasure. A domain
// of its own, so no apply, admin or maintainer signature can ever be replayed
// as an erasure, nor an erasure as one of them. The agent's
// packages/gate/src/session-action.ts holds the other half: change both or
// neither.
const AgentErasureDomain = "grainlify-account-erasure:v1\n"

const agentErasureTTL = 10 * time.Minute

// AgentErasureMessage is the exact text signed. Same seven-line shape as
// handlers.BountyActionMessage, with the erasure headline.
func AgentErasureMessage(login string, githubUserID int64, nonce string, issued, expires time.Time) string {
	return strings.Join([]string{
		"Grainlify: erase account",
		"Action: erase",
		fmt.Sprintf("GitHub: %s (id %d)", login, githubUserID),
		"Subject: ",
		"Nonce: " + nonce,
		"Issued: " + issued.UTC().Format(time.RFC3339),
		"Expires: " + expires.UTC().Format(time.RFC3339),
	}, "\n")
}

func (s *Services) EraseAtAgent(ctx context.Context, githubUserID int64, login string) error {
	if s.AgentKey == nil || s.AgentURL == "" {
		return ErrNotConfigured
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("erasure: nonce: %w", err)
	}
	issued := s.Now().UTC().Truncate(time.Second)
	msg := AgentErasureMessage(login, githubUserID, hex.EncodeToString(raw), issued, issued.Add(agentErasureTTL))
	sig := ed25519.Sign(s.AgentKey, []byte(AgentErasureDomain+msg))
	body, _ := json.Marshal(map[string]string{"message": msg, "countersignature": base64.StdEncoding.EncodeToString(sig)})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.AgentURL+"/account/erase", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("erasure: agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.AgentHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("erasure: agent unreachable: %w", err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusConflict:
		return ErrAgentInFlight
	default:
		// The agent's refusals carry an error code and no personal data.
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(out, &e)
		return fmt.Errorf("erasure: agent refused: status %d %s", res.StatusCode, e.Error)
	}
}
