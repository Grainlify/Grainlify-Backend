package api

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

type Deps struct {
	DB  *db.DB
	Bus bus.Bus
}

func New(cfg config.Config, deps Deps, build handlers.BuildInfo) *fiber.App {
	slog.Info("initializing Fiber app",
		"app_name", "grainlify-api",
	)
	// Since Fiber/fasthttp enforces BodyLimit at the server level before routing,
	// the global BodyLimit must accommodate the larger webhook payload size (e.g. 10 MB).
	// We then enforce the tighter MaxBodyBytes on all other routes via middleware.
	webhookBodyLimit := 10 * 1024 * 1024 // 10 MB for GitHub webhooks
	globalBodyLimit := cfg.MaxBodyBytes
	if globalBodyLimit < webhookBodyLimit {
		globalBodyLimit = webhookBodyLimit
	}

	app := fiber.New(fiber.Config{
		AppName:                 "grainlify-api",
		IdleTimeout:             60 * time.Second,
		ReadTimeout:             10 * time.Second,
		WriteTimeout:            10 * time.Second,
		BodyLimit:               globalBodyLimit,
		ErrorHandler:            JSONErrorHandler(),
		ProxyHeader:             fiber.HeaderXForwardedFor,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          cfg.TrustedProxies,
	})
	slog.Info("Fiber app created")

	// Baseline middleware.
	app.Use(requestid.New())
	app.Use(NewRateLimitMiddleware(cfg))

	// Add request logging middleware BEFORE recover to catch all requests
	app.Use(func(c *fiber.Ctx) error {
		// Log all incoming requests for debugging (especially webhooks)
		if strings.HasPrefix(c.Path(), "/webhooks/") {
			slog.Debug("webhook request received",
				"method", c.Method(),
				"path", c.Path(),
				"original_url", c.OriginalURL(),
				"remote_ip", c.IP(),
				"user_agent", c.Get("User-Agent"),
				"content_type", c.Get("Content-Type"),
				"content_length", c.Get("Content-Length"),
			)
		}
		return c.Next()
	})

	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: PanicStackTraceHandler,
	}))

	// Configure CORS from environment variables
	corsPolicy := BuildCORSOriginPolicy(cfg)
	corsConfig := cors.Config{
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization, X-Admin-Bootstrap-Token",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowCredentials: true,
		AllowOriginsFunc: corsPolicy.Allows,
	}

	app.Use(cors.New(corsConfig))
	app.Use(logger.New())

	// Enforce MAX_BODY_BYTES request body size limit for all routes except the GitHub webhook route
	app.Use(func(c *fiber.Ctx) error {
		// A non-positive limit means "no per-route limit configured"; skip the
		// check so an unset MaxBodyBytes does not reject every request.
		if cfg.MaxBodyBytes <= 0 {
			return c.Next()
		}

		path := c.Path()
		// Allow larger payloads on the GitHub webhook route
		if strings.HasPrefix(path, "/webhooks/github") {
			return c.Next()
		}

		// Enforce MaxBodyBytes limit by checking Content-Length header first
		contentLength := c.Request().Header.ContentLength()
		if contentLength > cfg.MaxBodyBytes {
			return fiber.ErrRequestEntityTooLarge
		}

		// Also check the actual read body size in case of chunked encoding or forged headers
		if len(c.Body()) > cfg.MaxBodyBytes {
			return fiber.ErrRequestEntityTooLarge
		}

		return c.Next()
	})

	// Routes.
	// Root handler - also handle POST requests to catch misconfigured webhooks
	app.Get("/", func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"service": "grainlify-api",
			"status":  "running",
			"version": "1.0.0",
		})
	})
	app.Post("/", func(c *fiber.Ctx) error {
		// Log POST requests to root - this helps identify if webhook URL is misconfigured
		slog.Warn("POST request received at root path - webhook URL might be misconfigured",
			"user_agent", c.Get("User-Agent"),
			"x_github_event", c.Get("X-GitHub-Event"),
			"x_github_delivery", c.Get("X-GitHub-Delivery"),
			"remote_ip", c.IP(),
		)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":       "webhook_url_misconfigured",
			"message":     "Webhook requests should be sent to /webhooks/github, not /",
			"correct_url": "/webhooks/github",
		})
	})
	app.Get("/health", handlers.NewHealth(build))
	app.Get("/ready", handlers.NewReady(deps.DB, deps.Bus))

	authHandler := handlers.NewAuthHandler(cfg, deps.DB)
	authGroup := app.Group("/auth")
	authGroup.Post("/nonce", authHandler.Nonce())
	authGroup.Post("/verify", authHandler.Verify())
	app.Get("/me", auth.RequireAuth(cfg.JWTSecret), authHandler.Me())
	app.Post("/me/github/resync", auth.RequireAuth(cfg.JWTSecret), authHandler.ResyncGitHubProfile())

	// User profile endpoints
	userProfile := handlers.NewUserProfileHandler(cfg, deps.DB)
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), userProfile.Profile())
	app.Get("/profile/public", userProfile.PublicProfile()) // Public profile endpoint (no auth required)
	app.Get("/profile/calendar", auth.RequireAuth(cfg.JWTSecret), userProfile.ContributionCalendar())
	app.Get("/profile/activity", auth.RequireAuth(cfg.JWTSecret), userProfile.ContributionActivity())
	app.Get("/profile/projects", auth.RequireAuth(cfg.JWTSecret), userProfile.ProjectsContributed())
	app.Get("/profile/projects-led", auth.RequireAuth(cfg.JWTSecret), userProfile.ProjectsLed())
	app.Put("/profile/update", auth.RequireAuth(cfg.JWTSecret), userProfile.UpdateProfile())
	app.Put("/profile/avatar", auth.RequireAuth(cfg.JWTSecret), userProfile.UpdateAvatar())

	ghOAuth := handlers.NewGitHubOAuthHandler(cfg, deps.DB)
	// GitHub-only login/signup:
	authGroup.Get("/github/login/start", ghOAuth.LoginStart())
	// Alias to unified callback (for backwards compatibility with older callback URLs).
	authGroup.Get("/github/login/callback", ghOAuth.CallbackUnified())

	// Legacy "link GitHub to existing account" endpoints (still available).
	authGroup.Post("/github/start", auth.RequireAuth(cfg.JWTSecret), ghOAuth.Start())
	authGroup.Get("/github/callback", ghOAuth.CallbackUnified())
	authGroup.Get("/github/status", auth.RequireAuth(cfg.JWTSecret), ghOAuth.Status())

	// GitHub App installation endpoints
	ghApp := handlers.NewGitHubAppHandler(cfg, deps.DB)
	authGroup.Post("/github/app/install/start", auth.RequireAuth(cfg.JWTSecret), ghApp.StartInstallation())
	app.Get("/auth/github/app/install/callback", ghApp.HandleInstallationCallback())

	// KYC verification endpoints
	kyc := handlers.NewKYCHandler(cfg, deps.DB)
	authGroup.Post("/kyc/start", auth.RequireAuth(cfg.JWTSecret), kyc.Start())
	authGroup.Get("/kyc/status", auth.RequireAuth(cfg.JWTSecret), kyc.Status())

	// Public ecosystems list and detail (includes computed project_count and user_count).
	ecosystems := handlers.NewEcosystemsPublicHandler(deps.DB)
	app.Get("/ecosystems", ecosystems.ListActive())
	app.Get("/ecosystems/:id", ecosystems.GetByID())

	// Open Source Week (public)
	osw := handlers.NewOpenSourceWeekHandler(deps.DB)
	app.Get("/open-source-week/events", osw.ListPublic())
	app.Get("/open-source-week/events/:id", osw.GetPublic())

	// Public leaderboard
	leaderboard := handlers.NewLeaderboardHandler(deps.DB)
	app.Get("/leaderboard", leaderboard.Leaderboard())

	// Public landing stats
	landingStats := handlers.NewLandingStatsHandler(deps.DB)
	app.Get("/stats/landing", landingStats.Get())

	// Public projects list with filtering
	projectsPublic := handlers.NewProjectsPublicHandler(cfg, deps.DB)
	app.Get("/projects", projectsPublic.List())
	app.Get("/projects/recommended", projectsPublic.Recommended())
	app.Get("/projects/filters", projectsPublic.FilterOptions())

	projects := handlers.NewProjectsHandler(cfg, deps.DB)
	app.Post("/projects", auth.RequireAuth(cfg.JWTSecret), projects.Create())
	// IMPORTANT: /projects/mine and /projects/pending-setup must come BEFORE /projects/:id to avoid route conflict
	app.Get("/projects/mine", auth.RequireAuth(cfg.JWTSecret), projects.Mine())
	app.Get("/projects/pending-setup", auth.RequireAuth(cfg.JWTSecret), projects.PendingSetup())

	// These routes with :id must come AFTER specific routes like /projects/mine
	app.Get("/projects/:id", projectsPublic.Get())
	app.Put("/projects/:id/metadata", auth.RequireAuth(cfg.JWTSecret), projects.UpdateMetadata())
	app.Get("/projects/:id/issues/public", projectsPublic.IssuesPublic())
	app.Get("/projects/:id/prs/public", projectsPublic.PRsPublic())
	app.Post("/projects/:id/verify", auth.RequireAuth(cfg.JWTSecret), projects.Verify())

	sync := handlers.NewSyncHandler(deps.DB)
	app.Post("/projects/:id/sync", auth.RequireAuth(cfg.JWTSecret), sync.EnqueueFullSync())
	app.Get("/projects/:id/sync/jobs", auth.RequireAuth(cfg.JWTSecret), sync.JobsForProject())

	data := handlers.NewProjectDataHandler(deps.DB)
	app.Get("/projects/:id/issues", auth.RequireAuth(cfg.JWTSecret), data.Issues())
	app.Get("/projects/:id/prs", auth.RequireAuth(cfg.JWTSecret), data.PRs())
	app.Get("/projects/:id/events", auth.RequireAuth(cfg.JWTSecret), data.Events())

	issueApps := handlers.NewIssueApplicationsHandler(cfg, deps.DB)
	app.Post("/projects/:id/issues/:number/apply", auth.RequireAuth(cfg.JWTSecret), issueApps.Apply())
	app.Post("/projects/:id/issues/:number/bot-comment", auth.RequireAuth(cfg.JWTSecret), issueApps.PostBotComment())
	app.Post("/projects/:id/issues/:number/withdraw", auth.RequireAuth(cfg.JWTSecret), issueApps.Withdraw())
	app.Post("/projects/:id/issues/:number/assign", auth.RequireAuth(cfg.JWTSecret), issueApps.Assign())
	app.Post("/projects/:id/issues/:number/unassign", auth.RequireAuth(cfg.JWTSecret), issueApps.Unassign())
	app.Post("/projects/:id/issues/:number/reject", auth.RequireAuth(cfg.JWTSecret), issueApps.Reject())

	admin := handlers.NewAdminHandler(cfg, deps.DB)
	adminGroup := app.Group("/admin", auth.RequireAuth(cfg.JWTSecret))
	adminGroup.Post("/bootstrap", admin.BootstrapAdmin())
	adminGroup.Get("/users", auth.RequireRole("admin"), admin.ListUsers())
	adminGroup.Put("/users/:id/role", auth.RequireRole("admin"), admin.SetUserRole())

	ecosystemsAdmin := handlers.NewEcosystemsAdminHandler(deps.DB)
	adminGroup.Get("/ecosystems", auth.RequireRole("admin"), ecosystemsAdmin.List())
	adminGroup.Get("/ecosystems/:id", auth.RequireRole("admin"), ecosystemsAdmin.GetByID())
	adminGroup.Post("/ecosystems", auth.RequireRole("admin"), ecosystemsAdmin.Create())
	adminGroup.Put("/ecosystems/:id", auth.RequireRole("admin"), ecosystemsAdmin.Update())
	adminGroup.Delete("/ecosystems/:id", auth.RequireRole("admin"), ecosystemsAdmin.Delete())

	// Open Source Week (admin)
	oswAdmin := handlers.NewOpenSourceWeekAdminHandler(deps.DB)
	adminGroup.Get("/open-source-week/events", auth.RequireRole("admin"), oswAdmin.List())
	adminGroup.Post("/open-source-week/events", auth.RequireRole("admin"), oswAdmin.Create())
	adminGroup.Delete("/open-source-week/events/:id", auth.RequireRole("admin"), oswAdmin.Delete())

	webhooks := handlers.NewGitHubWebhooksHandler(cfg, deps.DB, deps.Bus)
	// Register webhook endpoint with explicit OPTIONS support for CORS
	app.Options("/webhooks/github", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	// Also handle trailing slash
	app.Options("/webhooks/github/", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Post("/webhooks/github", webhooks.Receive())
	app.Post("/webhooks/github/", webhooks.Receive())

	// Didit webhook handler (supports both GET callback redirects and POST webhook events)
	diditWebhook := handlers.NewDiditWebhookHandler(cfg, deps.DB)
	app.Get("/webhooks/didit", diditWebhook.Receive())
	app.Post("/webhooks/didit", diditWebhook.Receive())

	// Add catch-all 404 handler to log unmatched routes (helps debug routing issues)
	app.Use(func(c *fiber.Ctx) error {
		slog.Warn("unmatched route",
			"method", c.Method(),
			"path", c.Path(),
			"original_url", c.OriginalURL(),
			"remote_ip", c.IP(),
			"user_agent", c.Get("User-Agent"),
		)
		return WriteErrorEnvelope(c, fiber.StatusNotFound, "not_found", "", fiber.Map{
			"path": c.Path(),
		})
	})

	slog.Info("all routes registered",
		"total_routes", "~30",
		"db_configured", deps.DB != nil,
		"nats_configured", deps.Bus != nil,
	)
	// Docs routes
	RegisterDocsRoutes(app)

	return app
}
