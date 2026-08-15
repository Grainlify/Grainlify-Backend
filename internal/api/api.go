package api

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/email"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type Deps struct {
	DB  *db.DB
	Bus bus.Bus
}

func New(cfg config.Config, deps Deps) *fiber.App {
	slog.Info("initializing Fiber app",
		"app_name", "grainlify-api",
	)
	app := fiber.New(fiber.Config{
		AppName:      "grainlify-api",
		IdleTimeout:  60 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		// Default is 4MB. Raised so a ~5MB bug-report screenshot (matching
		// the frontend's own upload cap) still fits after base64 inflation
		// (~1.37x) plus JSON overhead, instead of Fiber itself rejecting the
		// request with a raw 413 before it reaches any handler-level check.
		BodyLimit: 8 * 1024 * 1024,
	})
	slog.Info("Fiber app created")

	// Baseline middleware.
	app.Use(requestid.New())

	// Add request logging middleware BEFORE recover to catch all requests
	app.Use(func(c *fiber.Ctx) error {
		// Log all incoming requests for debugging (especially webhooks)
		if strings.HasPrefix(c.Path(), "/webhooks/") {
			slog.Info("webhook request received",
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

	app.Use(recover.New())

	// Configure CORS from environment variables
	corsConfig := cors.Config{
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization, X-Admin-Bootstrap-Token",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowCredentials: true,
	}

	// Always use AllowOriginsFunc so we can:
	// - allow localhost for dev
	// - allow explicit CORS_ORIGINS (comma-separated)
	// - allow FrontendBaseURL
	explicitOrigins := map[string]struct{}{}
	if strings.TrimSpace(cfg.CORSOrigins) != "" {
		for _, o := range strings.Split(cfg.CORSOrigins, ",") {
			o = strings.TrimSpace(o)
			if o == "" {
				continue
			}
			explicitOrigins[o] = struct{}{}
		}
	}

	corsConfig.AllowOriginsFunc = func(origin string) bool {
		// Always allow localhost origins for development / local frontend testing.
		if strings.HasPrefix(origin, "http://localhost:") ||
			strings.HasPrefix(origin, "http://127.0.0.1:") ||
			strings.HasPrefix(origin, "https://localhost:") ||
			strings.HasPrefix(origin, "https://127.0.0.1:") {
			return true
		}

		// Allow all Vercel preview deployments (*.vercel.app)
		if strings.HasSuffix(origin, ".vercel.app") {
			return true
		}

		// Allow production domain (*.0xo.in) for grainlify.0xo.in / api.grainlify.0xo.in
		if strings.HasSuffix(origin, ".0xo.in") {
			return true
		}

		// Allow the new production domain (grainlify.com and its subdomains,
		// e.g. www.grainlify.com) - kept alongside .0xo.in above during the
		// migration rather than replacing it, so the still-live .0xo.in
		// frontend doesn't lose CORS access before DNS/OAuth app settings for
		// grainlify.com are actually cut over.
		if origin == "https://grainlify.com" || strings.HasSuffix(origin, ".grainlify.com") {
			return true
		}

		// Check explicit CORS origins from config
		if _, ok := explicitOrigins[origin]; ok {
			return true
		}

		// If FrontendBaseURL is set, allow it (exact match or with path)
		if cfg.FrontendBaseURL != "" {
			frontendBase := strings.TrimSuffix(cfg.FrontendBaseURL, "/")
			if origin == frontendBase || strings.HasPrefix(origin, frontendBase+"/") {
				return true
			}
		}

		return false
	}

	app.Use(cors.New(corsConfig))
	app.Use(logger.New())

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
	app.Get("/health", handlers.Health())
	app.Get("/ready", handlers.Ready(deps.DB))

	// A nil *MailerCloudMailer boxed directly into the email.Mailer interface
	// would be a non-nil interface wrapping a nil pointer (classic Go
	// gotcha) - Service checks "mailer != nil" to decide whether to attempt
	// sending, so this must stay a genuine nil interface when disabled.
	var mailer email.Mailer
	if m := email.NewMailerCloudMailer(cfg.MailerCloudAPIKey, cfg.EmailFromAddress, cfg.EmailFromName); m != nil {
		mailer = m
	}
	notifSvc := notifications.New(deps.DB, mailer, cfg.FrontendBaseURL)

	authHandler := handlers.NewAuthHandler(cfg, deps.DB)
	authGroup := app.Group("/auth")
	app.Get("/me", auth.RequireAuth(cfg.JWTSecret), authHandler.Me())
	app.Post("/me/github/resync", auth.RequireAuth(cfg.JWTSecret), authHandler.ResyncGitHubProfile())

	// User profile endpoints
	userProfile := handlers.NewUserProfileHandler(cfg, deps.DB)
	app.Get("/profile", auth.RequireAuth(cfg.JWTSecret), userProfile.Profile())
	// Public profile: unauthenticated, and linkable - contributors share it to
	// show their standing, so it takes arbitrary ?login= strings from anyone.
	//
	// Each call runs internal/ranking twice (season and all-time), and
	// ranking.Position is a direct query that does NOT use the leaderboard
	// cache - it re-derives the whole ranking CTE and then filters to one row.
	// Before this endpoint stopped short-circuiting on unknown logins it
	// returned a stub instantly; now every arbitrary string costs two full
	// rankings, which is a real amplification vector on a public URL.
	//
	// Sharing the leaderboard cache would be better than limiting, and was the
	// first choice - but that cache lives on LeaderboardHandler, this handler
	// has no reference to it, and the nine construction sites deliberately vary
	// its TTL (tests use zero so they can read their own writes). Making it a
	// package-level global would remove that seam. So: bound the abuse here,
	// and treat cache sharing as the follow-up it deserves to be.
	//
	// 30/minute per IP is far above a human reading profiles and far below
	// what makes the endpoint useful as an amplifier.
	app.Get("/profile/public", limiter.New(limiter.Config{
		Max:        30,
		Expiration: 1 * time.Minute,
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "rate_limited"})
		},
	}), userProfile.PublicProfile())
	app.Get("/profile/calendar", auth.RequireAuth(cfg.JWTSecret), userProfile.ContributionCalendar())
	app.Get("/profile/activity", auth.RequireAuth(cfg.JWTSecret), userProfile.ContributionActivity())
	app.Get("/profile/projects", auth.RequireAuth(cfg.JWTSecret), userProfile.ProjectsContributed())
	app.Get("/profile/projects-led", auth.RequireAuth(cfg.JWTSecret), userProfile.ProjectsLed())
	app.Put("/profile/update", auth.RequireAuth(cfg.JWTSecret), userProfile.UpdateProfile())
	app.Put("/profile/avatar", auth.RequireAuth(cfg.JWTSecret), userProfile.UpdateAvatar())

	// Org profile + ratings endpoints
	orgRatings := handlers.NewOrgRatingsHandler(cfg, deps.DB)
	app.Get("/orgs/:login", orgRatings.Summary())
	app.Get("/orgs/:login/activity", orgRatings.Activity())
	app.Get("/orgs/:login/calendar", orgRatings.Calendar())
	app.Get("/orgs/:login/ratings", orgRatings.List())
	app.Get("/orgs/:login/ratings/me", auth.RequireAuth(cfg.JWTSecret), orgRatings.MyStatus())
	app.Post("/orgs/:login/ratings", auth.RequireAuth(cfg.JWTSecret), orgRatings.Submit())

	orgLinks := handlers.NewOrgLinksHandler(deps.DB)
	app.Get("/orgs/:login/links", orgLinks.Get())
	app.Put("/orgs/:login/links", auth.RequireAuth(cfg.JWTSecret), orgLinks.Update())

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
	kyc := handlers.NewKYCHandler(cfg, deps.DB, notifSvc)
	authGroup.Post("/kyc/start", auth.RequireAuth(cfg.JWTSecret), kyc.Start())
	authGroup.Get("/kyc/status", auth.RequireAuth(cfg.JWTSecret), kyc.Status())

	// Referral program: code + stats for the caller (internal/handlers/referrals.go).
	referrals := handlers.NewReferralsHandler(deps.DB, cfg)
	app.Get("/referrals/me", auth.RequireAuth(cfg.JWTSecret), referrals.Me())
	// Public: signs a referral code with its capture time so the published
	// 30-day window is enforced server-side rather than only in the browser.
	app.Get("/referrals/capture", referrals.Capture())

	// Points balance (internal/handlers/points.go). The points programme is
	// frozen - this stays readable so an existing balance is never hidden,
	// but nothing grants and nothing redeems.
	points := handlers.NewPointsHandler(deps.DB)
	app.Get("/points/me", auth.RequireAuth(cfg.JWTSecret), points.Me())

	// Founding Contributor Pool (internal/handlers/founding.go). Wave
	// progress is public - it is the announcement's engine. Neither endpoint
	// returns a money figure: §6 forbids any per-person number, and settlement
	// results are stored and rendered nowhere.
	foundingH := handlers.NewFoundingHandler(deps.DB)
	app.Get("/founding/waves", foundingH.WaveProgress())
	app.Get("/founding/me", auth.RequireAuth(cfg.JWTSecret), foundingH.MyShares())

	// Social-follow program: proof-of-follow submissions + review (internal/handlers/social_follow.go).
	socialFollow := handlers.NewSocialFollowHandler(deps.DB, notifSvc)
	// One request covering both platforms. The old per-platform endpoint is
	// gone rather than deprecated: leaving it reachable would leave the
	// half-approved state reachable with it.
	app.Post("/social-follow/submit", auth.RequireAuth(cfg.JWTSecret), socialFollow.SubmitAll())
	app.Get("/social-follow/me", auth.RequireAuth(cfg.JWTSecret), socialFollow.Me())

	// Points -> USDC redemption requests (internal/handlers/redemptions.go).
	redemptions := handlers.NewRedemptionsHandler(deps.DB, notifSvc)
	app.Post("/redemptions", auth.RequireAuth(cfg.JWTSecret), redemptions.Create())
	app.Get("/redemptions/me", auth.RequireAuth(cfg.JWTSecret), redemptions.Mine())

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
	app.Get("/leaderboard/projects", leaderboard.LeaderboardProjects())

	// Global search (projects, issues, contributors)
	search := handlers.NewSearchHandler(deps.DB)
	app.Get("/search", search.Search())

	// Public landing stats
	landingStats := handlers.NewLandingStatsHandler(deps.DB)
	app.Get("/stats/landing", landingStats.Get())

	// Bug reports: public, unauthenticated, relays straight to Discord (no
	// DB persistence). Rate-limited since it's an anonymous-reachable route
	// that fans out to a third-party webhook - Discord itself will throttle
	// or flag the webhook if hit too fast.
	bugReports := handlers.NewBugReportsHandler(cfg)
	app.Post("/bug-reports", limiter.New(limiter.Config{
		Max:        5,
		Expiration: 1 * time.Minute,
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "rate_limited"})
		},
	}), bugReports.Create())

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

	issueApps := handlers.NewIssueApplicationsHandler(cfg, deps.DB, notifSvc)
	app.Post("/projects/:id/issues/:number/apply", auth.RequireAuth(cfg.JWTSecret), issueApps.Apply())
	app.Post("/projects/:id/issues/:number/bot-comment", auth.RequireAuth(cfg.JWTSecret), issueApps.PostBotComment())
	app.Post("/projects/:id/issues/:number/withdraw", auth.RequireAuth(cfg.JWTSecret), issueApps.Withdraw())
	app.Post("/projects/:id/issues/:number/assign", auth.RequireAuth(cfg.JWTSecret), issueApps.Assign())
	app.Post("/projects/:id/issues/:number/unassign", auth.RequireAuth(cfg.JWTSecret), issueApps.Unassign())
	app.Post("/projects/:id/issues/:number/reject", auth.RequireAuth(cfg.JWTSecret), issueApps.Reject())
	app.Get("/issue-applications/me", auth.RequireAuth(cfg.JWTSecret), issueApps.Mine())

	// GrainHack (AI-specs.md) - Slice 1: hackathon lifecycle, project
	// applications, GitHub-label issue intake. Public + authenticated
	// contributor/maintainer routes here; admin routes are below with the
	// rest of adminGroup.
	hackathonPublic := handlers.NewHackathonPublicHandler(deps.DB)
	// Public rules page data - renders from live config (or a live event's
	// frozen snapshot) so it cannot drift from what is actually running.
	grainhackRules := handlers.NewGrainHackRulesHandler(deps.DB)
	app.Get("/grainhack/rules", grainhackRules.Rules())

	app.Get("/hackathons", hackathonPublic.List())
	app.Get("/hackathons/:id", hackathonPublic.GetByID())

	hackathonApps := handlers.NewHackathonApplicationsHandler(deps.DB)
	app.Post("/hackathons/:id/applications", auth.RequireAuth(cfg.JWTSecret), hackathonApps.Apply())
	app.Get("/hackathon-applications/me", auth.RequireAuth(cfg.JWTSecret), hackathonApps.Mine())

	hackathonIssues := handlers.NewHackathonIssuesHandler(deps.DB)
	app.Get("/projects/:id/hackathon-issues", auth.RequireAuth(cfg.JWTSecret), hackathonIssues.ListForProject())
	app.Get("/projects/:id/hackathon-issues/:number", auth.RequireAuth(cfg.JWTSecret), hackathonIssues.Get())
	app.Put("/projects/:id/hackathon-issues/:number", auth.RequireAuth(cfg.JWTSecret), hackathonIssues.UpdateFields())

	// §4 assignment pipeline, contributor-facing.
	hackathonIssueApps := handlers.NewHackathonIssueApplicationsHandler(cfg, deps.DB)
	// Contributor-visible issue state - any signed-in user, unlike the
	// owner-or-admin /projects/:id/hackathon-issues/:number above.
	app.Get("/projects/:id/grainhack/:number", auth.RequireAuth(cfg.JWTSecret), hackathonIssueApps.GetForContributor())
	app.Post("/hackathon-issues/:id/apply", auth.RequireAuth(cfg.JWTSecret), hackathonIssueApps.Apply())
	app.Get("/hackathon-issue-applications/me", auth.RequireAuth(cfg.JWTSecret), hackathonIssueApps.Mine())
	app.Get("/hackathon-assignments/me", auth.RequireAuth(cfg.JWTSecret), hackathonIssueApps.MyAssignments())
	app.Post("/hackathon-assignments/:id/release", auth.RequireAuth(cfg.JWTSecret), hackathonIssueApps.Release())

	// §7 issue-clarity rating. Optional and skippable: nothing here sits on
	// the path to submitting a PR. Maintainers see aggregates only, and only
	// once the event has closed.
	hackathonClarity := handlers.NewHackathonClarityHandler(deps.DB)
	app.Post("/hackathon-assignments/:id/clarity-rating", auth.RequireAuth(cfg.JWTSecret), hackathonClarity.Submit())
	app.Get("/projects/:id/grainhack/clarity", auth.RequireAuth(cfg.JWTSecret), hackathonClarity.ForMaintainer())

	// §6 appeals, contributor side. my-verdicts returns nothing until results
	// are published - the verdict view and the appeal window open together.
	hackathonAppeals := handlers.NewHackathonAppealsHandler(deps.DB)
	app.Get("/grainhack/my-verdicts", auth.RequireAuth(cfg.JWTSecret), hackathonAppeals.MyVerdicts())
	app.Get("/grainhack/verdicts/:id/appeal-window", auth.RequireAuth(cfg.JWTSecret), hackathonAppeals.AppealWindowForVerdict())
	app.Post("/grainhack/verdicts/:id/appeal", auth.RequireAuth(cfg.JWTSecret), hackathonAppeals.Appeal())

	admin := handlers.NewAdminHandler(cfg, deps.DB)
	// Admin authorisation reads the role from the database on every request
	// rather than trusting the JWT's role claim, so removing someone's admin
	// takes effect immediately instead of when their token expires. Uncached
	// by design - see auth.RequireLiveRole.
	liveRole := handlers.NewRoleLookup(deps.DB)
	requireAdmin := auth.RequireLiveRole(liveRole, "admin")

	adminGroup := app.Group("/admin", auth.RequireAuth(cfg.JWTSecret))
	adminGroup.Get("/users", requireAdmin, admin.ListUsers())
	adminGroup.Put("/users/:id/role", requireAdmin, admin.SetUserRole())

	// Admin KYC reset. A refused verification is terminal in the UI
	// (canStartNewKYCSession allows only "", expired and not_started), so
	// without this the only way to unblock a contributor is an UPDATE against
	// production - which is how four of them were unblocked, invisibly.
	// Audited to kyc_reset_audit: who reset whom, from what status, and why.
	kycAdmin := handlers.NewKYCAdminHandler(deps.DB, notifSvc)
	adminGroup.Post("/kyc/:id/reset", requireAdmin, kycAdmin.Reset())
	adminGroup.Get("/kyc/:id/resets", requireAdmin, kycAdmin.History())

	adminGroup.Get("/social-follow/submissions", requireAdmin, socialFollow.ListSubmissions())
	adminGroup.Post("/social-follow/submissions/:id/approve", requireAdmin, socialFollow.Approve())
	adminGroup.Post("/social-follow/submissions/:id/reject", requireAdmin, socialFollow.Reject())
	// Eligibility is re-read at settlement, so an approval has to be
	// withdrawable after the fact.
	adminGroup.Post("/social-follow/submissions/:id/revoke", requireAdmin, socialFollow.Revoke())

	adminGroup.Get("/redemptions", requireAdmin, redemptions.ListAdmin())
	adminGroup.Post("/redemptions/:id/mark-paid", requireAdmin, redemptions.MarkPaid())
	adminGroup.Post("/redemptions/:id/reject", requireAdmin, redemptions.Reject())

	ecosystemsAdmin := handlers.NewEcosystemsAdminHandler(deps.DB)
	adminGroup.Get("/ecosystems", requireAdmin, ecosystemsAdmin.List())
	adminGroup.Get("/ecosystems/:id", requireAdmin, ecosystemsAdmin.GetByID())
	adminGroup.Post("/ecosystems", requireAdmin, ecosystemsAdmin.Create())
	adminGroup.Put("/ecosystems/:id", requireAdmin, ecosystemsAdmin.Update())
	adminGroup.Delete("/ecosystems/:id", requireAdmin, ecosystemsAdmin.Delete())

	// Open Source Week (admin)
	oswAdmin := handlers.NewOpenSourceWeekAdminHandler(deps.DB)
	adminGroup.Get("/open-source-week/events", requireAdmin, oswAdmin.List())
	adminGroup.Post("/open-source-week/events", requireAdmin, oswAdmin.Create())
	adminGroup.Delete("/open-source-week/events/:id", requireAdmin, oswAdmin.Delete())

	// GrainHack (admin)
	adminHackathons := handlers.NewAdminHackathonsHandler(deps.DB)
	adminGroup.Post("/hackathons", requireAdmin, adminHackathons.Create())
	adminGroup.Get("/hackathons", requireAdmin, adminHackathons.List())
	adminGroup.Get("/hackathons/:id", requireAdmin, adminHackathons.GetByID())
	adminGroup.Put("/hackathons/:id", requireAdmin, adminHackathons.Update())
	adminGroup.Post("/hackathons/:id/transition", requireAdmin, adminHackathons.Transition())

	adminHackathonApps := handlers.NewAdminHackathonApplicationsHandler(cfg, deps.DB, notifSvc)
	adminGroup.Get("/hackathons/:id/applications", requireAdmin, adminHackathonApps.ListAdmin())
	adminGroup.Get("/hackathons/applications/:appId/signals", requireAdmin, adminHackathonApps.Signals())
	adminGroup.Post("/hackathons/applications/:appId/accept", requireAdmin, adminHackathonApps.Accept())
	adminGroup.Post("/hackathons/applications/:appId/reject", requireAdmin, adminHackathonApps.Reject())
	adminGroup.Post("/hackathons/applications/:appId/request-more-info", requireAdmin, adminHackathonApps.RequestMoreInfo())

	adminGroup.Get("/hackathons/:id/issues", requireAdmin, hackathonIssues.ListForHackathon())

	// §4 assignment pipeline, admin-facing. simulate-draw runs the full
	// pipeline against real applicants and writes no assignment.
	adminDraws := handlers.NewAdminHackathonDrawsHandler(deps.DB)
	adminGroup.Post("/hackathon-issues/:id/simulate-draw", requireAdmin, adminDraws.Simulate())
	adminGroup.Get("/hackathons/:id/draws", requireAdmin, adminDraws.ListDraws())
	adminGroup.Get("/hackathons/:id/assignments", requireAdmin, adminDraws.ListAssignments())

	// §5 judging - the human review interface. In shadow mode (the default)
	// every verdict is reviewed by hand, so this is the primary surface.
	adminVerdicts := handlers.NewAdminHackathonVerdictsHandler(deps.DB)
	adminGroup.Get("/hackathons/:id/verdicts", requireAdmin, adminVerdicts.List())
	adminGroup.Get("/hackathon-verdicts/:id", requireAdmin, adminVerdicts.Get())
	adminGroup.Post("/hackathon-verdicts/:id/override", requireAdmin, adminVerdicts.Override())

	// §6 appeals. The admin queue carries the full verdict with each appeal so
	// a reviewer answers it with both model verdicts and the diff in front of
	// them, as the spec requires.
	adminGroup.Get("/hackathons/:id/appeals", requireAdmin, hackathonAppeals.AdminList())
	adminGroup.Post("/hackathon-appeals/:id/decide", requireAdmin, hackathonAppeals.AdminDecide())

	// §2.3 step 4 - out-of-band assignments surfaced for review. Advisory:
	// crossing the threshold flags an org, it does not penalise one.
	adminOOB := handlers.NewAdminHackathonOOBHandler(deps.DB)
	adminGroup.Get("/hackathons/:id/oob-assignments", requireAdmin, adminOOB.List())

	adminHackathonConfig := handlers.NewAdminHackathonConfigHandler(deps.DB)
	adminGroup.Get("/hackathon-config", requireAdmin, adminHackathonConfig.List())
	adminGroup.Put("/hackathon-config", requireAdmin, adminHackathonConfig.Update())
	adminGroup.Post("/hackathon-config/reset", requireAdmin, adminHackathonConfig.Reset())
	adminGroup.Get("/hackathon-config/audit", requireAdmin, adminHackathonConfig.Audit())

	// Notifications (in-app list/read + per-type email/in-app preferences)
	notif := handlers.NewNotificationsHandler(deps.DB)
	notifGroup := app.Group("/notifications", auth.RequireAuth(cfg.JWTSecret))
	notifGroup.Get("/", notif.List())
	notifGroup.Get("/unread-count", notif.UnreadCount())
	notifGroup.Post("/read-all", notif.MarkAllRead())
	notifGroup.Post("/:id/read", notif.MarkRead())
	notifGroup.Get("/preferences", notif.GetPreferences())
	notifGroup.Put("/preferences", notif.UpdatePreferences())

	webhooks := handlers.NewGitHubWebhooksHandler(cfg, deps.DB, deps.Bus, notifSvc)
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
	diditWebhook := handlers.NewDiditWebhookHandler(cfg, deps.DB, notifSvc)
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
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "not_found",
			"path":  c.Path(),
		})
	})

	slog.Info("all routes registered",
		"total_routes", "~30",
		"db_configured", deps.DB != nil,
		"nats_configured", deps.Bus != nil,
	)

	return app
}
