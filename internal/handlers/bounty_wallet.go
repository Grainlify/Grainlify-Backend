package handlers

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// BountyWalletHandler countersigns a Solana wallet link for Grainlify Bounties.
//
// The bounty agent (a separate service, github.com/Grainlify/grainlify-bounty-agent)
// pays bounties to the Solana wallet linked to a GitHub account. It needs to
// know that a link request comes from the person signed in to Grainlify as that
// GitHub account, and it must not hold anything that could mint a Grainlify
// session - JWT_SECRET would let it sign in as anybody, admins included.
//
// So this endpoint is the only thing Grainlify contributes: it reads WHO is
// signed in, writes that into a short message together with the wallet, a
// random nonce and a ten-minute expiry, and signs the message with a key that
// exists for nothing else. The wallet then signs the same message. The agent
// holds only the public half of this key; it checks both signatures, the
// expiry, and that the nonce has never been used, and stores the link.
//
// Nothing here writes to the database, and nothing here touches payout
// addresses: that is contributor_addresses (the KeeperHub programme), a
// different chain family and a different system. A countersignature on its
// own links nothing - without the wallet's signature it is inert.
type BountyWalletHandler struct {
	db  *db.DB
	key ed25519.PrivateKey // nil when unconfigured: the endpoint answers 503
	now func() time.Time
}

// bountyLinkDomain prefixes what the countersigning key signs, so a signature
// from it can never be read as meaning anything but this.
const bountyLinkDomain = "grainlify-bounty-wallet-link:v1\n"

// bountyReadDomain is deliberately a DIFFERENT domain from the link one. Both
// are signed by the same key, so the domain is the only thing stopping a
// signature issued for one purpose being presented as the other. A read
// challenge proves "this GitHub account is asking"; it must never be usable to
// link a wallet, which is why it carries no Wallet line and no wallet
// signature is ever accepted alongside it.
const bountyReadDomain = "grainlify-bounty-wallet-read:v1\n"

// bountyLinkTTL is how long a countersigned message may be used. Long enough to
// switch to a wallet app and back; short enough that a leaked one is stale.
const bountyLinkTTL = 10 * time.Minute

// NewBountyWalletHandler takes the signing key as base64 of the 32-byte ed25519
// seed (BOUNTY_LINK_SIGNING_KEY). An empty or malformed key leaves the endpoint
// answering 503 rather than failing the whole API at boot: this feature is
// optional, and sign-in must never depend on it.
func NewBountyWalletHandler(d *db.DB, keyB64 string) *BountyWalletHandler {
	h := &BountyWalletHandler{db: d, now: time.Now}
	if strings.TrimSpace(keyB64) == "" {
		return h
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(seed) != ed25519.SeedSize {
		slog.Error("BOUNTY_LINK_SIGNING_KEY is not base64 of a 32-byte ed25519 seed; bounty wallet linking is off")
		return h
	}
	h.key = ed25519.NewKeyFromSeed(seed)
	return h
}

// BountyLinkMessage is the exact text both Grainlify and the wallet sign. The
// agent parses it back, so its shape is a contract with the agent's
// packages/gate/src/session-link.ts: change both or neither.
func BountyLinkMessage(login string, githubUserID int64, wallet, nonce string, issued, expires time.Time) string {
	return strings.Join([]string{
		"Grainlify: link this wallet to my GitHub account",
		fmt.Sprintf("GitHub: %s (id %d)", login, githubUserID),
		"Wallet: " + wallet,
		"Nonce: " + nonce,
		"Issued: " + issued.UTC().Format(time.RFC3339),
		"Expires: " + expires.UTC().Format(time.RFC3339),
	}, "\n")
}

// BountyReadMessage is what the agent parses to answer "which wallet is linked
// to this GitHub account". Five lines, no Wallet: the caller is asking, not
// asserting. Its shape is a contract with the agent's
// packages/gate/src/session-read.ts: change both or neither.
func BountyReadMessage(login string, githubUserID int64, nonce string, issued, expires time.Time) string {
	return strings.Join([]string{
		"Grainlify: read my linked wallet",
		fmt.Sprintf("GitHub: %s (id %d)", login, githubUserID),
		"Nonce: " + nonce,
		"Issued: " + issued.UTC().Format(time.RFC3339),
		"Expires: " + expires.UTC().Format(time.RFC3339),
	}, "\n")
}

// GetCountersignKey answers GET /bounty-wallet/countersign-key with the public
// half of the key that signs link and read challenges.
//
// Public on purpose and safe to expose: a public key verifies signatures and
// cannot make them. It exists so the pairing between this service and the
// bounty agent can be checked from outside, without reading either service's
// environment -- a mismatch there produces bad_countersignature, which is
// otherwise indistinguishable from a genuinely bad request.
func (h *BountyWalletHandler) GetCountersignKey(c *fiber.Ctx) error {
	if h.key == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bounty_wallet_link_unconfigured"})
	}
	pub := h.key.Public().(ed25519.PublicKey)
	return c.JSON(fiber.Map{
		"public_key":  base64.StdEncoding.EncodeToString(pub),
		"link_domain": bountyLinkDomain,
		"read_domain": bountyReadDomain,
	})
}

// PostReadChallenge answers POST /me/bounty-wallet/read-challenge. It takes no
// body: the GitHub account comes from the session, and reading your own link
// needs no proof of wallet control, only proof of who is asking.
func (h *BountyWalletHandler) PostReadChallenge(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	if h.key == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bounty_wallet_link_unconfigured"})
	}

	var githubUserID int64
	var login string
	err := h.db.Pool.QueryRow(c.Context(),
		`SELECT github_user_id, login FROM github_accounts WHERE user_id = $1`, uid).Scan(&githubUserID, &login)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "github_not_linked"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "nonce_failed"})
	}
	nonce := hex.EncodeToString(raw)
	issued := h.now().UTC().Truncate(time.Second)
	expires := issued.Add(bountyLinkTTL)
	msg := BountyReadMessage(login, githubUserID, nonce, issued, expires)
	sig := ed25519.Sign(h.key, []byte(bountyReadDomain+msg))

	return c.JSON(fiber.Map{
		"message":          msg,
		"countersignature": base64.StdEncoding.EncodeToString(sig),
		"expires_at":       expires,
	})
}

// PostChallenge answers POST /me/bounty-wallet/challenge {"wallet": "<base58>"}.
func (h *BountyWalletHandler) PostChallenge(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	if h.key == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bounty_wallet_link_unconfigured"})
	}
	var body struct {
		Wallet string `json:"wallet"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	wallet := strings.TrimSpace(body.Wallet)
	if !isSolanaAddress(wallet) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_solana_address"})
	}

	// The GitHub account comes from the session's own record, never from the
	// request: that is the whole point of the endpoint.
	var githubUserID int64
	var login string
	err := h.db.Pool.QueryRow(c.Context(),
		`SELECT github_user_id, login FROM github_accounts WHERE user_id = $1`, uid).Scan(&githubUserID, &login)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "github_not_linked"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "nonce_failed"})
	}
	nonce := hex.EncodeToString(raw)
	issued := h.now().UTC().Truncate(time.Second)
	expires := issued.Add(bountyLinkTTL)
	msg := BountyLinkMessage(login, githubUserID, wallet, nonce, issued, expires)
	sig := ed25519.Sign(h.key, []byte(bountyLinkDomain+msg))

	return c.JSON(fiber.Map{
		"message":          msg,
		"countersignature": base64.StdEncoding.EncodeToString(sig),
		"expires_at":       expires,
	})
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isSolanaAddress: base58 that decodes to exactly 32 bytes. Whether the key is
// on the curve does not matter here - the wallet's signature decides that.
func isSolanaAddress(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	n := new(big.Int)
	for _, r := range s {
		i := strings.IndexRune(base58Alphabet, r)
		if i < 0 {
			return false
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return zeros+len(n.Bytes()) == 32
}
