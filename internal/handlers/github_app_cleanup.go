package handlers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// GitHubAppCleanupHandler handles periodic cleanup of uninstalled GitHub Apps
type GitHubAppCleanupHandler struct {
	cfg  config.Config
	pool db.DBPool
}

func NewGitHubAppCleanupHandler(cfg config.Config, pool db.DBPool) *GitHubAppCleanupHandler {
	return &GitHubAppCleanupHandler{
		cfg:  cfg,
		pool: pool,
	}
}

// RunPeriodicCleanup runs a background task that periodically checks if installations are still active
// and marks projects as deleted if the installation no longer exists
func (h *GitHubAppCleanupHandler) RunPeriodicCleanup(ctx context.Context) {
	if h.cfg.GitHubAppID == "" || h.cfg.GitHubAppPrivateKey == "" {
		slog.Warn("GitHub App not configured, skipping periodic cleanup")
		return
	}

	ticker := time.NewTicker(5 * time.Minute) // Check every 5 minutes
	defer ticker.Stop()

	slog.Info("GitHub App periodic cleanup started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("GitHub App periodic cleanup stopped")
			return
		case <-ticker.C:
			h.checkInstallations(ctx)
		}
	}
}

// checkInstallations checks all active installations and marks projects as deleted if installation is gone
func (h *GitHubAppCleanupHandler) checkInstallations(ctx context.Context) {
	if h.pool == nil {
		return
	}

	// Get all unique installation IDs from projects that aren't deleted
	rows, err := h.pool.Query(ctx, `
SELECT DISTINCT github_app_installation_id
FROM projects
WHERE github_app_installation_id IS NOT NULL
  AND deleted_at IS NULL
`)
	if err != nil {
		slog.Error("failed to query installations", "error", err)
		return
	}
	defer rows.Close()

	var installationIDs []string
	for rows.Next() {
		var installationID string
		if err := rows.Scan(&installationID); err != nil {
			continue
		}
		installationIDs = append(installationIDs, installationID)
	}

	if len(installationIDs) == 0 {
		return
	}

	slog.Info("checking installation status",
		"count", len(installationIDs),
	)

	// Create GitHub App client
	appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
	if err != nil {
		slog.Error("failed to create GitHub App client", "error", err)
		return
	}

	// Check each installation
	for _, installationID := range installationIDs {
		h.checkSingleInstallation(ctx, appClient, installationID)
	}
}

// installationTokenGetter is an interface that allows GetInstallationToken to be
// mocked in tests without standing up a real GitHub App client.
type installationTokenGetter interface {
	GetInstallationToken(ctx context.Context, installationID string) (string, error)
}

// checkSingleInstallation checks if a single installation is still active.
//
// Detection strategy:
//   - If GetInstallationToken returns an error that wraps github.ErrInstallationNotFound
//     (i.e. an HTTP 404 from GitHub), the installation is genuinely gone and projects
//     are marked deleted.
//   - Any other error (network failure, 5xx, auth/permission error, etc.) is treated as
//     a transient failure: we log a warning and leave projects untouched to avoid
//     mass-deleting real, still-active projects.
//   - No error means the installation is healthy; nothing to do.
func (h *GitHubAppCleanupHandler) checkSingleInstallation(ctx context.Context, appClient installationTokenGetter, installationID string) {
	_, err := appClient.GetInstallationToken(ctx, installationID)
	if err == nil {
		// Installation is still active — nothing to do.
		return
	}

	if !errors.Is(err, github.ErrInstallationNotFound) {
		// Transient or unrelated error (network, 5xx, auth, etc.).
		// Do NOT mark projects as deleted — log and continue.
		slog.Warn("failed to check installation status",
			"installation_id", installationID,
			"error", err,
		)
		return
	}

	// HTTP 404 confirmed: the installation was uninstalled.
	slog.Info("installation no longer exists, marking projects as deleted",
		"installation_id", installationID,
	)

	result, dbErr := h.pool.Exec(ctx, `
UPDATE projects
SET deleted_at = now(),
    status = 'rejected',
    updated_at = now()
WHERE github_app_installation_id = $1
  AND deleted_at IS NULL
`, installationID)
	if dbErr != nil {
		slog.Error("failed to mark projects as deleted",
			"installation_id", installationID,
			"error", dbErr,
		)
		return
	}

	slog.Info("marked projects as deleted",
		"installation_id", installationID,
		"rows_affected", result.RowsAffected(),
	)
}
