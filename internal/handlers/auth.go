package handlers

import (
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

const (
	// MaxWalletTypeLength defines the maximum allowed length of the wallet_type string.
	MaxWalletTypeLength = 50

	// MaxAddressLength defines the maximum allowed length of the address string.
	MaxAddressLength = 128

	// MaxNonceLength defines the maximum allowed length of the nonce string.
	MaxNonceLength = 128

	// MaxSignatureLength defines the maximum allowed length of the signature string.
	MaxSignatureLength = 256

	// MaxPublicKeyLength defines the maximum allowed length of the public_key string.
	MaxPublicKeyLength = 256
)

// isValidHex checks if a string is a valid hexadecimal representation.
// It allows an optional "0x" or "0X" prefix.
func isValidHex(s string) bool {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if len(s) == 0 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// isValidBase64 checks if a string contains only valid base64 or base64url characters.
func isValidBase64(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '+' || c == '/' || c == '-' || c == '_' || c == '=') {
			return false
		}
	}
	return true
}

// isValidNonce checks if a nonce is within valid length bounds and contains only valid base64url/alphanumeric characters.
func isValidNonce(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || len(trimmed) > MaxNonceLength {
		return false
	}
	return isValidBase64(trimmed)
}

// isValidAddress checks if a wallet address is valid and bounds its length/format per wallet type.
func isValidAddress(wType auth.WalletType, addr string) bool {
	trimmed := strings.TrimSpace(addr)
	if len(trimmed) == 0 || len(trimmed) > MaxAddressLength {
		return false
	}

	switch wType {
	case auth.WalletTypeEVM:
		// EVM address must be hex-encoded. Standard length is 40 hex characters,
		// plus optional 0x/0X prefix (so total 40 or 42 characters).
		hasPrefix := strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X")
		expectedLen := 40
		if hasPrefix {
			expectedLen = 42
		}
		if len(trimmed) != expectedLen {
			return false
		}
		return isValidHex(trimmed)

	case auth.WalletTypeStellarEd25519, auth.WalletTypeStellarSecp256k1:
		// For Stellar, address can be a base32 address (starts with G, M, etc., length 56),
		// or a public key hex. We check that it contains only alphanumeric characters
		// after an optional 0x/0X prefix, and has length between 5 and 128 characters.
		val := trimmed
		if strings.HasPrefix(strings.ToLower(val), "0x") {
			val = val[2:]
		}
		if len(val) < 5 {
			return false
		}
		for i := 0; i < len(val); i++ {
			c := val[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				return false
			}
		}
		return true

	default:
		return false
	}
}

// isValidSignature checks if a signature has a valid hex shape and size for the wallet type.
func isValidSignature(wType auth.WalletType, signature string) bool {
	trimmed := strings.TrimSpace(signature)
	if len(trimmed) == 0 || len(trimmed) > MaxSignatureLength {
		return false
	}

	switch wType {
	case auth.WalletTypeEVM:
		// EVM signature must be valid hex. Standard length is 65 bytes (130 hex chars + optional 0x).
		hasPrefix := strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X")
		expectedLen := 130
		if hasPrefix {
			expectedLen = 132
		}
		if len(trimmed) != expectedLen {
			return false
		}
		return isValidHex(trimmed)

	case auth.WalletTypeStellarEd25519:
		// Ed25519 signature is 64 bytes. In hex, that is 128 characters (optional 0x).
		hasPrefix := strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X")
		expectedLen := 128
		if hasPrefix {
			expectedLen = 130
		}
		if len(trimmed) != expectedLen {
			return false
		}
		return isValidHex(trimmed)

	case auth.WalletTypeStellarSecp256k1:
		// Secp256k1 signature can be compact (64 bytes / 128 hex chars) or DER (typically 70-72 bytes / 140-144 hex chars).
		// We validate it is hex and length is within a reasonable range (120 to 160 chars).
		if !isValidHex(trimmed) {
			return false
		}
		hexPart := strings.TrimPrefix(trimmed, "0x")
		hexPart = strings.TrimPrefix(hexPart, "0X")
		l := len(hexPart)
		return l == 128 || (l >= 120 && l <= 160)

	default:
		return false
	}
}

// isValidPublicKey checks if a public key is non-empty, within bounds, and has a valid format.
func isValidPublicKey(wType auth.WalletType, pubKey string) bool {
	trimmed := strings.TrimSpace(pubKey)
	if len(trimmed) == 0 || len(trimmed) > MaxPublicKeyLength {
		return false
	}

	switch wType {
	case auth.WalletTypeEVM:
		// Public key is ignored for EVM, but if provided, it should be a valid hex or base64 string
		// within length bounds.
		return isValidHex(trimmed) || isValidBase64(trimmed)

	case auth.WalletTypeStellarEd25519:
		// Stellar Ed25519 public key is 32 bytes (64 hex characters + optional 0x).
		hasPrefix := strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X")
		expectedLen := 64
		if hasPrefix {
			expectedLen = 66
		}
		if len(trimmed) != expectedLen {
			return false
		}
		return isValidHex(trimmed)

	case auth.WalletTypeStellarSecp256k1:
		// Secp256k1 public key can be compressed (33 bytes / 66 hex chars) or uncompressed (65 bytes / 130 hex chars).
		hexPart := strings.TrimPrefix(trimmed, "0x")
		hexPart = strings.TrimPrefix(hexPart, "0X")
		if !isValidHex(trimmed) {
			return false
		}
		l := len(hexPart)
		return l == 66 || l == 130

	default:
		return false
	}
}

type AuthHandler struct {
	cfg config.Config
	db  *db.DB
}

func NewAuthHandler(cfg config.Config, d *db.DB) *AuthHandler {
	return &AuthHandler{cfg: cfg, db: d}
}

type nonceRequest struct {
	WalletType string `json:"wallet_type"`
	Address    string `json:"address"`
}

func (h *AuthHandler) Nonce() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		var req nonceRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "")
		}

		if len(req.WalletType) > MaxWalletTypeLength {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_wallet_type", "")
		}

		wType, err := auth.NormalizeWalletType(req.WalletType)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_wallet_type", "")
		}

		if !isValidAddress(wType, req.Address) {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_address", "")
		}

		addr, err := auth.NormalizeAddress(wType, req.Address)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_address", "")
		}

		n, err := auth.CreateNonce(c.Context(), h.db.Pool, wType, addr, 10*time.Minute)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "nonce_create_failed", "")
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"nonce":      n.Nonce,
			"message":    auth.LoginMessage(n.Nonce),
			"expires_at": n.ExpiresAt,
		})
	}
}

type verifyRequest struct {
	WalletType string `json:"wallet_type"`
	Address    string `json:"address"`
	Nonce      string `json:"nonce"`
	Signature  string `json:"signature"`
	PublicKey  string `json:"public_key,omitempty"`
}

func (h *AuthHandler) Verify() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if h.cfg.JWTSecret == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "jwt_not_configured", "")
		}

		var req verifyRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "")
		}

		if len(req.WalletType) > MaxWalletTypeLength {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_wallet_type", "")
		}

		wType, err := auth.NormalizeWalletType(req.WalletType)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_wallet_type", "")
		}

		if !isValidAddress(wType, req.Address) {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_address", "")
		}

		addr, err := auth.NormalizeAddress(wType, req.Address)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_address", "")
		}

		if req.Nonce == "" || req.Signature == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "missing_nonce_or_signature", "")
		}

		if !isValidNonce(req.Nonce) {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_nonce", "")
		}

		if !isValidSignature(wType, req.Signature) {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_signature", "")
		}

		if wType == auth.WalletTypeStellarEd25519 || wType == auth.WalletTypeStellarSecp256k1 {
			if !isValidPublicKey(wType, req.PublicKey) {
				return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_public_key", "")
			}
		} else {
			if req.PublicKey != "" && !isValidPublicKey(wType, req.PublicKey) {
				return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_public_key", "")
			}
		}

		// Be tolerant during early dev: accept both the current canonical message and the
		// legacy newline message (so signing tools that copied `\n` vs newline don't block you).
		msgs := []string{
			auth.LoginMessage(req.Nonce),
			auth.LegacyLoginMessage(req.Nonce),
		}
		var sigOK bool
		for _, msg := range msgs {
			if err := auth.VerifySignature(wType, addr, msg, req.Signature, req.PublicKey); err == nil {
				sigOK = true
				break
			}
		}
		if !sigOK {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_signature", "")
		}

		res, err := auth.ConsumeNonceAndUpsertUser(c.Context(), h.db.Pool, wType, addr, req.Nonce, req.PublicKey)
		if err != nil {
			if err.Error() == "invalid_or_expired_nonce" {
				return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_or_expired_nonce", "")
			}
			return httpx.RespondError(c, fiber.StatusInternalServerError, "auth_failed", "")
		}

		token, err := auth.IssueJWT(h.cfg.JWTSecret, res.User.ID, res.User.Role, res.Wallet.WalletType, res.Wallet.Address, 15*time.Minute)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "token_issue_failed", "")
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"token": token,
			"user":  res.User,
			"wallet": fiber.Map{
				"wallet_type": res.Wallet.WalletType,
				"address":     res.Wallet.Address,
			},
		})
	}
}

func (h *AuthHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		role, _ := c.Locals(auth.LocalRole).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		// Get user profile fields from database
		var firstName, lastName, location, website, bio, avatarURL, telegram, linkedin, whatsapp, twitter, discord *string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT first_name, last_name, location, website, bio, avatar_url, telegram, linkedin, whatsapp, twitter, discord
FROM users
WHERE id = $1
`, userID).Scan(&firstName, &lastName, &location, &website, &bio, &avatarURL, &telegram, &linkedin, &whatsapp, &twitter, &discord)
		if err != nil {
			slog.Warn("failed to fetch user profile fields", "error", err, "user_id", userID)
		}

		response := fiber.Map{
			"id":   userIDStr,
			"role": role,
		}

		// Try to get GitHub access token and fetch full profile
		linkedAccount, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err == nil {
			// Fetch full GitHub user profile
			gh := github.NewClient()
			ghUser, err := gh.GetUser(c.Context(), linkedAccount.AccessToken)
			if err == nil {
				githubMap := fiber.Map{
					"login": ghUser.Login,
				}
				// Use database avatar_url if set, otherwise use GitHub avatar
				if avatarURL != nil && *avatarURL != "" {
					githubMap["avatar_url"] = *avatarURL
				} else {
					githubMap["avatar_url"] = ghUser.AvatarURL
				}
				// Add optional fields if available
				if ghUser.Name != "" {
					githubMap["name"] = ghUser.Name
				}
				// Try to get email from GitHub emails endpoint (more reliable)
				email, err := gh.GetPrimaryEmail(c.Context(), linkedAccount.AccessToken)
				if err == nil && email != "" {
					githubMap["email"] = email
				} else if ghUser.Email != "" {
					// Fallback to email from /user endpoint
					githubMap["email"] = ghUser.Email
				}
				// Use database location if set, otherwise use GitHub location
				if location != nil && *location != "" {
					githubMap["location"] = *location
				} else if ghUser.Location != "" {
					githubMap["location"] = ghUser.Location
				}
				// Use database bio if set, otherwise use GitHub bio
				if bio != nil && *bio != "" {
					githubMap["bio"] = *bio
				} else if ghUser.Bio != "" {
					githubMap["bio"] = ghUser.Bio
				}
				// Use database website if set, otherwise use GitHub blog
				if website != nil && *website != "" {
					githubMap["website"] = *website
				} else if ghUser.Blog != "" {
					githubMap["website"] = ghUser.Blog
				}
				response["github"] = githubMap
			} else {
				// Fallback to database values if GitHub API fails
				var githubLogin *string
				var githubAvatarURL *string
				_ = h.db.Pool.QueryRow(c.Context(), `
SELECT login, avatar_url
FROM github_accounts
WHERE user_id = $1
`, userID).Scan(&githubLogin, &githubAvatarURL)
				if githubLogin != nil {
					githubMap := fiber.Map{
						"login": *githubLogin,
					}
					// Use database avatar_url if set, otherwise use GitHub account avatar
					if avatarURL != nil && *avatarURL != "" {
						githubMap["avatar_url"] = *avatarURL
					} else if githubAvatarURL != nil && *githubAvatarURL != "" {
						githubMap["avatar_url"] = *githubAvatarURL
					}
					// Add profile fields from database
					if location != nil && *location != "" {
						githubMap["location"] = *location
					}
					if bio != nil && *bio != "" {
						githubMap["bio"] = *bio
					}
					if website != nil && *website != "" {
						githubMap["website"] = *website
					}
					response["github"] = githubMap
				}
			}
		} else {
			// No GitHub account linked, try to get from database anyway
			var githubLogin *string
			var githubAvatarURL *string
			_ = h.db.Pool.QueryRow(c.Context(), `
SELECT login, avatar_url
FROM github_accounts
WHERE user_id = $1
`, userID).Scan(&githubLogin, &githubAvatarURL)
			if githubLogin != nil {
				githubMap := fiber.Map{
					"login": *githubLogin,
				}
				// Use database avatar_url if set, otherwise use GitHub account avatar
				if avatarURL != nil && *avatarURL != "" {
					githubMap["avatar_url"] = *avatarURL
				} else if githubAvatarURL != nil && *githubAvatarURL != "" {
					githubMap["avatar_url"] = *githubAvatarURL
				}
				// Add profile fields from database
				if location != nil && *location != "" {
					githubMap["location"] = *location
				}
				if bio != nil && *bio != "" {
					githubMap["bio"] = *bio
				}
				if website != nil && *website != "" {
					githubMap["website"] = *website
				}
				response["github"] = githubMap
			}
		}

		// Add user profile fields to response (for first_name, last_name, social links)
		if firstName != nil && *firstName != "" {
			response["first_name"] = *firstName
		}
		if lastName != nil && *lastName != "" {
			response["last_name"] = *lastName
		}
		if telegram != nil && *telegram != "" {
			response["telegram"] = *telegram
		}
		if linkedin != nil && *linkedin != "" {
			response["linkedin"] = *linkedin
		}
		if whatsapp != nil && *whatsapp != "" {
			response["whatsapp"] = *whatsapp
		}
		if twitter != nil && *twitter != "" {
			response["twitter"] = *twitter
		}
		if discord != nil && *discord != "" {
			response["discord"] = *discord
		}

		return c.Status(fiber.StatusOK).JSON(response)
	}
}

// ResyncGitHubProfile fetches fresh GitHub profile data including email
func (h *AuthHandler) ResyncGitHubProfile() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		// Get GitHub access token
		linkedAccount, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusNotFound, "github_not_linked", "")
		}

		// Fetch fresh GitHub user profile
		gh := github.NewClient()
		ghUser, err := gh.GetUser(c.Context(), linkedAccount.AccessToken)
		if err != nil {
			slog.Error("failed to fetch GitHub user", "error", err, "user_id", userID)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "github_fetch_failed", "")
		}

		// Get primary email from GitHub
		email, err := gh.GetPrimaryEmail(c.Context(), linkedAccount.AccessToken)
		if err != nil {
			slog.Warn("failed to fetch GitHub email", "error", err, "user_id", userID)
			// Continue without email if email fetch fails
		}

		// Update github_accounts table with fresh data
		_, err = h.db.Pool.Exec(c.Context(), `
UPDATE github_accounts
SET login = $1, avatar_url = $2, updated_at = now()
WHERE user_id = $3
`, ghUser.Login, ghUser.AvatarURL, userID)
		if err != nil {
			slog.Error("failed to update github_accounts", "error", err, "user_id", userID)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "update_failed", "")
		}

		// Return fresh GitHub data
		githubMap := fiber.Map{
			"login":      ghUser.Login,
			"avatar_url": ghUser.AvatarURL,
		}
		if ghUser.Name != "" {
			githubMap["name"] = ghUser.Name
		}
		if email != "" {
			githubMap["email"] = email
		} else if ghUser.Email != "" {
			githubMap["email"] = ghUser.Email
		}
		if ghUser.Location != "" {
			githubMap["location"] = ghUser.Location
		}
		if ghUser.Bio != "" {
			githubMap["bio"] = ghUser.Bio
		}
		if ghUser.Blog != "" {
			githubMap["website"] = ghUser.Blog
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"github": githubMap,
		})
	}
}
