package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

type ProjectsHandler struct {
	cfg              config.Config
	db               *db.DB
	onProjectChanged func(projectID string) // callback to invalidate cache on project CUD operations

	// verifyInFlight tracks project IDs currently being processed by
	// verifyAndWebhook, keyed by uuid.UUID. It's a fast-path guard against
	// the common rapid-resubmit case (Issue #359) — the actual correctness
	// guarantee against duplicate webhook creation is the SELECT ... FOR
	// UPDATE re-check inside verifyAndWebhook, which holds regardless of
	// how many goroutines/instances race past this map.
	verifyInFlight sync.Map
}

func NewProjectsHandler(cfg config.Config, d *db.DB) *ProjectsHandler {
	return &ProjectsHandler{cfg: cfg, db: d}
}

// SetCacheInvalidator sets the callback to invalidate the projects public cache
// when projects are created, updated, or deleted.  This must be called after
// both the projects handler and the projects public handler are constructed.
func (h *ProjectsHandler) SetCacheInvalidator(fn func(projectID string)) {
	h.onProjectChanged = fn
}

type createProjectRequest struct {
	GitHubFullName string   `json:"github_full_name"`
	EcosystemName  string   `json:"ecosystem_name"` // Users provide name, not slug
	Language       *string  `json:"language,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Category       *string  `json:"category,omitempty"`
}

func (h *ProjectsHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		var req createProjectRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "")
		}

		fullName := normalizeRepoFullName(req.GitHubFullName)
		if fullName == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_github_full_name", "")
		}

		// Ecosystem is required (must be an active ecosystem from DB)
		ecosystemName := strings.TrimSpace(req.EcosystemName)
		if ecosystemName == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "ecosystem_required", "Ecosystem name is required")
		}

		var ecosystemID uuid.UUID
		// Search by name (case-insensitive, trimmed) - must be active
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT id
FROM ecosystems
WHERE LOWER(TRIM(name)) = LOWER(TRIM($1))
  AND status = 'active'
`, ecosystemName).Scan(&ecosystemID)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "ecosystem_not_found", "No active ecosystem found with that name. Please select from available ecosystems.")
		}

		// Prepare tags as JSONB
		var tagsJSON []byte = []byte("[]")
		if len(req.Tags) > 0 {
			tagsJSON, _ = json.Marshal(req.Tags)
		}

		var projectID uuid.UUID
		var status string
		// The DO UPDATE only fires when the existing row's owner already
		// matches the caller (idempotent re-submit). For a github_full_name
		// already owned by someone else, the WHERE condition is false, so
		// Postgres leaves that row completely untouched and RETURNING
		// yields no row — surfaced below as a 409, not a silent takeover.
		err = h.db.Pool.QueryRow(c.Context(), `
INSERT INTO projects (owner_user_id, github_full_name, ecosystem_id, language, tags, category, status)
VALUES ($1, $2, $3, $4, $5, $6, 'pending_verification')
ON CONFLICT (github_full_name) DO UPDATE SET
  ecosystem_id = EXCLUDED.ecosystem_id,
  language = EXCLUDED.language,
  tags = EXCLUDED.tags,
  category = EXCLUDED.category,
  updated_at = now()
WHERE projects.owner_user_id = EXCLUDED.owner_user_id
RETURNING id, status
`, userID, fullName, ecosystemID, req.Language, tagsJSON, req.Category).Scan(&projectID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusConflict, "project_already_registered", "This repository is already registered under a different account.")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_create_failed", "")
		}

		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"id":               projectID.String(),
			"github_full_name": fullName,
			"ecosystem_name":   ecosystemName,
			"status":           status,
		})
	}
}

func (h *ProjectsHandler) Mine() fiber.Handler {
	return func(c *fiber.Ctx) error {
		slog.Info("projects/mine: handler called",
			"method", c.Method(),
			"path", c.Path(),
			"request_id", c.Locals("requestid"),
		)

		if h.db == nil || h.db.Pool == nil {
			slog.Error("projects/mine: database not configured",
				"request_id", c.Locals("requestid"),
			)
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, ok := c.Locals(auth.LocalUserID).(string)
		if !ok || sub == "" {
			requestID := c.Locals("requestid")
			slog.Warn("projects/mine: missing or invalid user_id in context",
				"user_id_type", fmt.Sprintf("%T", c.Locals(auth.LocalUserID)),
				"user_id_value", c.Locals(auth.LocalUserID),
				"request_id", requestID,
			)
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		userID, err := uuid.Parse(sub)
		if err != nil {
			slog.Warn("projects/mine: failed to parse user_id as UUID",
				"user_id", sub,
				"error", err,
				"request_id", c.Locals("requestid"),
			)
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		slog.Info("projects/mine: querying projects",
			"user_id", userID.String(),
			"request_id", c.Locals("requestid"),
		)

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT 
  p.id, 
  p.github_full_name, 
  p.status, 
  p.github_repo_id, 
  p.verified_at, 
  p.verification_error, 
  p.webhook_id, 
  p.webhook_url, 
  p.webhook_created_at, 
  p.created_at, 
  p.updated_at,
  e.name AS ecosystem_name,
  p.language,
  p.tags,
  p.category,
  p.description,
  p.needs_metadata
FROM projects p
LEFT JOIN ecosystems e ON p.ecosystem_id = e.id
WHERE p.owner_user_id = $1
  AND p.deleted_at IS NULL
ORDER BY p.created_at DESC
`, userID)
		if err != nil {
			slog.Error("projects/mine: database query failed",
				"user_id", userID.String(),
				"error", err,
				"request_id", c.Locals("requestid"),
			)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "projects_list_failed", "")
		}
		defer rows.Close()

		// Scan every row first so the GitHub repo-metadata fetch below can run
		// once, concurrently, across all rows instead of once per row
		// sequentially (issue #290).
		type mineRow struct {
			id                                uuid.UUID
			fullName, status                  string
			repoID                            *int64
			verifiedAt, webhookCreatedAt      *time.Time
			verErr, webhookURL                *string
			webhookID                         *int64
			createdAt, updatedAt              time.Time
			ecosystemName, language, category *string
			tagsJSON                          []byte
			description                       *string
			needsMetadata                     bool
		}
		var mineRows []mineRow
		for rows.Next() {
			var r mineRow
			if err := rows.Scan(&r.id, &r.fullName, &r.status, &r.repoID, &r.verifiedAt, &r.verErr, &r.webhookID, &r.webhookURL, &r.webhookCreatedAt, &r.createdAt, &r.updatedAt, &r.ecosystemName, &r.language, &r.tagsJSON, &r.category, &r.description, &r.needsMetadata); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "projects_list_failed", "")
			}
			mineRows = append(mineRows, r)
		}
		if err := rows.Err(); err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "projects_list_failed", "")
		}

		// Get user's GitHub access token for fetching repo data
		linkedAccount, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		var accessToken string
		if err == nil {
			accessToken = linkedAccount.AccessToken
		}

		gh := github.NewClient()
		var repoResults map[string]repoFetchResult
		if accessToken != "" {
			fullNames := make([]string, len(mineRows))
			for i, r := range mineRows {
				fullNames[i] = r.fullName
			}
			repoResults = fetchReposConcurrently(c.Context(), gh, accessToken, fullNames)
		}

		var out []fiber.Map
		for _, r := range mineRows {
			// Fetch repo data from GitHub to check if it's private and get owner avatar
			var ownerAvatarURL *string
			var isPrivate bool
			if accessToken != "" {
				res := repoResults[r.fullName]
				if res.err == nil {
					isPrivate = res.repo.Private
					if !isPrivate {
						ownerAvatarURL = &res.repo.Owner.AvatarURL
					}
				} else {
					// If we can't fetch (404/403), assume it's private
					isPrivate = true
				}
			}

			// Skip private repos
			if isPrivate {
				// Soft delete private repos from database
				_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE projects
SET deleted_at = now()
WHERE id = $1
`, r.id)
				continue
			}

			// Parse tags JSONB
			var tags []string
			if len(r.tagsJSON) > 0 {
				_ = json.Unmarshal(r.tagsJSON, &tags)
			}

			projectMap := fiber.Map{
				"id":                 r.id.String(),
				"github_full_name":   r.fullName,
				"status":             r.status,
				"github_repo_id":     r.repoID,
				"verified_at":        r.verifiedAt,
				"verification_error": r.verErr,
				"webhook_id":         r.webhookID,
				"webhook_url":        r.webhookURL,
				"webhook_created_at": r.webhookCreatedAt,
				"created_at":         r.createdAt,
				"updated_at":         r.updatedAt,
				"ecosystem_name":     r.ecosystemName,
				"language":           r.language,
				"tags":               tags,
				"category":           r.category,
				"description":        r.description,
				"needs_metadata":     r.needsMetadata,
			}

			// Add owner avatar if available
			if ownerAvatarURL != nil {
				projectMap["owner_avatar_url"] = *ownerAvatarURL
			}

			out = append(out, projectMap)
		}

		// Always return an array, even if empty
		if out == nil {
			out = []fiber.Map{}
		}

		slog.Info("projects/mine: returning projects",
			"user_id", userID.String(),
			"count", len(out),
			"request_id", c.Locals("requestid"),
		)

		return c.Status(fiber.StatusOK).JSON(out)
	}
}

// PendingSetup returns projects for the current user that need metadata (needs_metadata = true).
func (h *ProjectsHandler) PendingSetup() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT p.id, p.github_full_name, p.description, p.ecosystem_id, e.name AS ecosystem_name, p.language, p.tags, p.category
FROM projects p
LEFT JOIN ecosystems e ON p.ecosystem_id = e.id
WHERE p.owner_user_id = $1
  AND p.needs_metadata = true
  AND p.deleted_at IS NULL
ORDER BY p.created_at ASC
`, userID)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "pending_setup_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var id uuid.UUID
			var fullName string
			var description *string
			var ecosystemID *uuid.UUID
			var ecosystemName *string
			var language *string
			var tagsJSON []byte
			var category *string

			if err := rows.Scan(&id, &fullName, &description, &ecosystemID, &ecosystemName, &language, &tagsJSON, &category); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "pending_setup_failed", "")
			}

			var tags []string
			if len(tagsJSON) > 0 {
				_ = json.Unmarshal(tagsJSON, &tags)
			}

			ecoID := ""
			if ecosystemID != nil {
				ecoID = ecosystemID.String()
			}
			ecoName := ""
			if ecosystemName != nil {
				ecoName = *ecosystemName
			}

			out = append(out, fiber.Map{
				"id":               id.String(),
				"github_full_name": fullName,
				"description":      description,
				"ecosystem_id":     ecoID,
				"ecosystem_name":   ecoName,
				"language":         language,
				"tags":             tags,
				"category":         category,
			})
		}
		if err := rows.Err(); err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "pending_setup_failed", "")
		}

		if out == nil {
			out = []fiber.Map{}
		}
		return c.Status(fiber.StatusOK).JSON(out)
	}
}

type updateMetadataRequest struct {
	Description   *string  `json:"description,omitempty"`
	EcosystemName *string  `json:"ecosystem_name,omitempty"`
	Language      *string  `json:"language,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Category      *string  `json:"category,omitempty"`
}

// UpdateMetadata updates project metadata and sets needs_metadata = false.
func (h *ProjectsHandler) UpdateMetadata() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}

		var req updateMetadataRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "")
		}

		var ownerUserID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id FROM projects WHERE id = $1 AND deleted_at IS NULL
`, projectID).Scan(&ownerUserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}
		if ownerUserID != userID {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}

		// Resolve ecosystem if name provided
		var ecosystemID *uuid.UUID
		if req.EcosystemName != nil && strings.TrimSpace(*req.EcosystemName) != "" {
			var ecoID uuid.UUID
			err := h.db.Pool.QueryRow(c.Context(), `
SELECT id FROM ecosystems WHERE LOWER(TRIM(name)) = LOWER(TRIM($1)) AND status = 'active'
`, *req.EcosystemName).Scan(&ecoID)
			if err != nil {
				return httpx.RespondError(c, fiber.StatusBadRequest, "ecosystem_not_found", "No active ecosystem found with that name.")
			}
			ecosystemID = &ecoID
		}

		var tagsJSON []byte = []byte("[]")
		if len(req.Tags) > 0 {
			tagsJSON, _ = json.Marshal(req.Tags)
		}

		// Build dynamic update: set needs_metadata = false and provided fields
		_, err = h.db.Pool.Exec(c.Context(), `
UPDATE projects
SET description = COALESCE($2, description),
    ecosystem_id = COALESCE($3, ecosystem_id),
    language = COALESCE($4, language),
    tags = COALESCE($5, tags),
    category = COALESCE($6, category),
    needs_metadata = false,
    updated_at = now()
WHERE id = $1
`, projectID, req.Description, ecosystemID, req.Language, tagsJSON, req.Category)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "metadata_update_failed", "")
		}

		// Invalidate projects cache since project metadata changed
		if h.onProjectChanged != nil {
			h.onProjectChanged(projectID.String())
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

func (h *ProjectsHandler) Verify() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		role, _ := c.Locals(auth.LocalRole).(string)

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}

		var ownerUserID uuid.UUID
		var fullName string
		var webhookID *int64
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, webhook_id
FROM projects
WHERE id = $1
`, projectID).Scan(&ownerUserID, &fullName, &webhookID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}

		if ownerUserID != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}

		// Fast-path guard: if a verify is already running for this project,
		// don't spawn a second goroutine for a rapid double-click/retry.
		// This does not by itself prevent duplicate webhook creation (see
		// the SELECT ... FOR UPDATE re-check in verifyAndWebhook for the
		// actual correctness guarantee) — it just avoids the wasted work
		// and GitHub API calls in the common case.
		if _, alreadyRunning := h.verifyInFlight.LoadOrStore(projectID, struct{}{}); alreadyRunning {
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"queued": true, "already_in_progress": true})
		}

		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE projects
SET status = 'pending_verification', verification_error = NULL, updated_at = now()
WHERE id = $1
`, projectID)

		// Async job (in-process for now): return immediately per architecture rule.
		go h.verifyAndWebhook(context.Background(), projectID, ownerUserID, fullName, webhookID)

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"queued": true})
	}
}

func (h *ProjectsHandler) verifyAndWebhook(ctx context.Context, projectID uuid.UUID, ownerUserID uuid.UUID, fullName string, existingWebhookID *int64) {
	defer h.verifyInFlight.Delete(projectID)

	// Keep this best-effort and resilient; failures should be recorded on the project.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if h.db == nil || h.db.Pool == nil {
		return
	}

	linked, err := github.GetLinkedAccount(ctx, h.db.Pool, ownerUserID, h.cfg.TokenEncKeyB64)
	if err != nil {
		h.recordProjectError(ctx, projectID, "github_not_linked")
		return
	}

	gh := github.NewClient()
	repo, err := gh.GetRepo(ctx, linked.AccessToken, fullName)
	if err != nil {
		h.recordProjectError(ctx, projectID, fmt.Sprintf("repo_fetch_failed: %v", err))
		return
	}

	// Ownership/permission check: allow if the token has admin or push perms.
	if !repo.Permissions.Admin && !repo.Permissions.Push {
		h.recordProjectError(ctx, projectID, "insufficient_repo_permissions (need admin or push)")
		return
	}

	// If webhook already exists, just mark verified.
	if existingWebhookID != nil && *existingWebhookID != 0 {
		_, _ = h.db.Pool.Exec(ctx, `
UPDATE projects
SET github_repo_id = $2,
    status = 'verified',
    verified_at = now(),
    verification_error = NULL,
    stars_count = $3,
    forks_count = $4,
    updated_at = now()
WHERE id = $1
`, projectID, repo.ID, repo.StargazersCount, repo.ForksCount)
		// Invalidate cache since project is now verified and visible
		if h.onProjectChanged != nil {
			h.onProjectChanged(projectID.String())
		}
		return
	}

	if h.cfg.PublicBaseURL == "" || h.cfg.GitHubWebhookSecret == "" {
		h.recordProjectError(ctx, projectID, "webhook_not_configured (PUBLIC_BASE_URL and GITHUB_WEBHOOK_SECRET required)")
		return
	}

	webhookURL := strings.TrimRight(h.cfg.PublicBaseURL, "/") + "/webhooks/github"

	// existingWebhookID reflects whatever Verify() observed when it enqueued
	// this goroutine, which can be stale if a second concurrent verify
	// request for the same project raced this one (Issue #359) — both would
	// otherwise see webhook_id = NULL and each create a separate GitHub
	// webhook. Re-check under a row lock, and hold that lock across the
	// CreateWebhook call and the subsequent write, so a second transaction
	// racing this one blocks until this one commits and then sees its
	// result instead of also creating a webhook.
	txErr := db.WithTx(ctx, h.db.Pool, func(tx pgx.Tx) error {
		var lockedWebhookID *int64
		if err := tx.QueryRow(ctx, `
SELECT webhook_id FROM projects WHERE id = $1 FOR UPDATE
`, projectID).Scan(&lockedWebhookID); err != nil {
			return fmt.Errorf("lock project row: %w", err)
		}

		if lockedWebhookID != nil && *lockedWebhookID != 0 {
			// Another concurrent verify already created (or is finishing)
			// the webhook between our initial read and now — mark verified
			// without creating a second one.
			_, err := tx.Exec(ctx, `
UPDATE projects
SET github_repo_id = $2,
    status = 'verified',
    verified_at = now(),
    verification_error = NULL,
    stars_count = $3,
    forks_count = $4,
    updated_at = now()
WHERE id = $1
`, projectID, repo.ID, repo.StargazersCount, repo.ForksCount)
			return err
		}

		wh, err := gh.CreateWebhook(ctx, linked.AccessToken, fullName, github.CreateWebhookRequest{
			URL:    webhookURL,
			Secret: h.cfg.GitHubWebhookSecret,
			Events: []string{"issues", "pull_request", "pull_request_review", "push"},
			Active: true,
		})
		if err != nil {
			return fmt.Errorf("webhook_create_failed: %w", err)
		}

		_, err = tx.Exec(ctx, `
UPDATE projects
SET github_repo_id = $2,
    status = 'verified',
    verified_at = now(),
    verification_error = NULL,
    webhook_id = $3,
    webhook_url = $4,
    webhook_created_at = now(),
    stars_count = $5,
    forks_count = $6,
    updated_at = now()
WHERE id = $1
`, projectID, repo.ID, wh.ID, webhookURL, repo.StargazersCount, repo.ForksCount)
		return err
	})
	if txErr != nil {
		h.recordProjectError(ctx, projectID, txErr.Error())
		return
	}

	// Invalidate cache since project is now verified and visible
	if h.onProjectChanged != nil {
		h.onProjectChanged(projectID.String())
	}
}

func (h *ProjectsHandler) recordProjectError(ctx context.Context, projectID uuid.UUID, msg string) {
	_, _ = h.db.Pool.Exec(ctx, `
UPDATE projects
SET verification_error = $2,
    status = 'pending_verification',
    updated_at = now()
WHERE id = $1
`, projectID, msg)
}

func normalizeRepoFullName(v string) string {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "http://github.com/")
	s = strings.TrimSuffix(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return ""
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
