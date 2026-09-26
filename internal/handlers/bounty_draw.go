package handlers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// BountyDrawHandler lets a signed-in contributor apply for a bounty, and an
// admin drive the draw, without either one ever talking to the bounty agent.
//
// The trust model is the wallet link's, reused: this service is the only one
// that knows who is signed in, so it writes a short message naming the person
// and the action and signs it with the bounty link key. The agent holds the
// public half, verifies, and acts. The browser carries the signed message but
// cannot make one.
//
// The split of responsibility is worth stating because it is easy to get
// backwards. WHO the caller is, the signature proves. WHETHER they may run an
// admin action is decided HERE, before signing, by the same requireAdmin that
// guards every other admin route - the agent does not have a user table and
// should not grow one. The two domains below are what stop a contributor's
// apply signature being presented as an admin one.
type BountyDrawHandler struct {
	db        *db.DB
	key       ed25519.PrivateKey
	now       func() time.Time
	agentURL  string
	agentHTTP *http.Client
}

// Distinct domains over one key. A signature is only ever valid for the thing
// it was made for; see packages/gate/src/session-action.ts, which refuses on
// the domain before it looks at anything else.
const (
	bountyApplyDomain = "grainlify-bounty-apply:v1\n"
	bountyAdminDomain = "grainlify-bounty-admin:v1\n"
)

const bountyActionTTL = 10 * time.Minute

func NewBountyDrawHandler(d *db.DB, keyB64, agentURL string) *BountyDrawHandler {
	h := &BountyDrawHandler{db: d, now: time.Now, agentURL: agentURL, agentHTTP: &http.Client{Timeout: 15 * time.Second}}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(seed) != ed25519.SeedSize {
		if strings.TrimSpace(keyB64) != "" {
			slog.Error("BOUNTY_LINK_SIGNING_KEY is not base64 of a 32-byte ed25519 seed; the bounty draw is off")
		}
		return h
	}
	h.key = ed25519.NewKeyFromSeed(seed)
	return h
}

// BountyActionMessage is the exact text this service signs. Its shape is a
// contract with the agent's packages/gate/src/session-action.ts: change both
// or neither. Subject is constrained by the agent's parser to a character set
// with no newline in it, so nothing a caller supplies can add a line.
func BountyActionMessage(kind, action, login string, githubUserID int64, subject, nonce string, issued, expires time.Time) string {
	headline := "apply for a bounty"
	if kind == "admin" {
		headline = "admin action"
	}
	return strings.Join([]string{
		"Grainlify: " + headline,
		"Action: " + action,
		fmt.Sprintf("GitHub: %s (id %d)", login, githubUserID),
		"Subject: " + subject,
		"Nonce: " + nonce,
		"Issued: " + issued.UTC().Format(time.RFC3339),
		"Expires: " + expires.UTC().Format(time.RFC3339),
	}, "\n")
}

// subjectSafe mirrors the agent's Subject character class. Checked here as
// well as there so a bad subject is a clear 400 from the service that built
// the message, rather than an opaque "malformed" from the one that parsed it.
func subjectSafe(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == ':' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (h *BountyDrawHandler) githubFor(c *fiber.Ctx) (int64, string, error) {
	uid, ok := userID(c)
	if !ok {
		return 0, "", errors.New("unauthenticated")
	}
	var id int64
	var login string
	err := h.db.Pool.QueryRow(c.Context(),
		`SELECT github_user_id, login FROM github_accounts WHERE user_id = $1`, uid).Scan(&id, &login)
	return id, login, err
}

// call signs a message and relays it to the agent, returning the agent's
// status and body unchanged. The agent's refusals are written for the person
// who asked; rewording them here would only lose detail.
func (h *BountyDrawHandler) call(c *fiber.Ctx, domain, kind, action string, githubUserID int64, login, subject string, extra map[string]any) error {
	if h.key == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bounty_draw_unconfigured"})
	}
	if !subjectSafe(subject) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_subject"})
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "nonce_failed"})
	}
	issued := h.now().UTC().Truncate(time.Second)
	msg := BountyActionMessage(kind, action, login, githubUserID, subject, hex.EncodeToString(raw), issued, issued.Add(bountyActionTTL))
	sig := ed25519.Sign(h.key, []byte(domain+msg))

	body := map[string]any{"message": msg, "countersignature": base64.StdEncoding.EncodeToString(sig)}
	for k, v := range extra {
		// Never let a caller overwrite the two fields that carry the identity.
		if k == "message" || k == "countersignature" {
			continue
		}
		body[k] = v
	}
	payload, _ := json.Marshal(body)

	path := "/bounties/apply"
	if kind == "admin" {
		path = "/admin/draw"
	}
	req, err := http.NewRequestWithContext(c.Context(), "POST", strings.TrimRight(h.agentURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "agent_request_failed"})
	}
	req.Header.Set("content-type", "application/json")
	res, err := h.agentHTTP.Do(req)
	if err != nil {
		slog.Warn("bounty draw: agent unreachable", "agent_url", h.agentURL, "action", action, "error", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "agent_unreachable", "agent_url": h.agentURL})
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 512*1024))
	c.Set("content-type", "application/json")
	return c.Status(res.StatusCode).Send(out)
}

// PostApply applies the signed-in contributor to a bounty.
func (h *BountyDrawHandler) PostApply(c *fiber.Ctx) error {
	id, login, err := h.githubFor(c)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "github_not_linked"})
	}
	if err != nil {
		if err.Error() == "unauthenticated" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return h.call(c, bountyApplyDomain, "apply", "apply", id, login, c.Params("bountyId"), nil)
}

// adminAction is the shared body of every admin route below. requireAdmin has
// already run; this only needs the admin's identity for the audit trail the
// agent keeps (bounty_config.updated_by, bounty_draws.triggered_by).
func (h *BountyDrawHandler) adminAction(c *fiber.Ctx, action, subject string, extra map[string]any) error {
	id, login, err := h.githubFor(c)
	if err != nil {
		if err.Error() == "unauthenticated" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
		}
		// An admin without a linked GitHub account still has an identity worth
		// recording; "admin" alone would make the audit trail useless.
		if !errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
		}
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "github_not_linked", "detail": "link a GitHub account first, so draw actions are attributable"})
	}
	return h.call(c, bountyAdminDomain, "admin", action, id, login, subject, extra)
}

func (h *BountyDrawHandler) GetSettings(c *fiber.Ctx) error {
	return h.adminAction(c, "list_settings", "", nil)
}

func (h *BountyDrawHandler) PostSetting(c *fiber.Ctx) error {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.BodyParser(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	return h.adminAction(c, "set_setting", in.Key, map[string]any{"value": in.Value})
}

func (h *BountyDrawHandler) PostSettingReset(c *fiber.Ctx) error {
	var in struct {
		Key string `json:"key"`
	}
	if err := c.BodyParser(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	return h.adminAction(c, "reset_setting", in.Key, nil)
}

func (h *BountyDrawHandler) GetBountyState(c *fiber.Ctx) error {
	return h.adminAction(c, "bounty_state", c.Params("bountyId"), nil)
}

func (h *BountyDrawHandler) PostRunDraw(c *fiber.Ctx) error {
	var in struct {
		Simulate bool `json:"simulate"`
	}
	// A body is optional here: no body means a real draw.
	_ = c.BodyParser(&in)
	return h.adminAction(c, "run_draw", c.Params("bountyId"), map[string]any{"simulate": in.Simulate})
}
