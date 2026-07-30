package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

func TestValidateAvatarURL(t *testing.T) {
	// A small valid 1x1 PNG data URI, well under maxAvatarDataURILen.
	const validPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

	tests := []struct {
		name      string
		avatarURL string
		wantErr   bool
	}{
		{name: "https URL accepted", avatarURL: "https://example.com/avatar.png"},
		{name: "http URL accepted", avatarURL: "http://example.com/avatar.png"},
		{name: "valid data:image/png accepted", avatarURL: validPNG},
		{name: "data:image/jpeg accepted", avatarURL: "data:image/jpeg;base64,/9j/4AAQSkZJRg=="},
		{name: "data:image/gif accepted", avatarURL: "data:image/gif;base64,R0lGODlhAQABAAAAACw="},
		{name: "data:image/webp accepted", avatarURL: "data:image/webp;base64,UklGRhoAAABXRUJQVlA4="},
		{name: "data:image/svg+xml rejected", avatarURL: "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", wantErr: true},
		{name: "data:text/plain rejected", avatarURL: "data:text/plain;base64,aGVsbG8=", wantErr: true},
		{name: "data:image/svg+xml without base64 flag rejected", avatarURL: "data:image/svg+xml,<svg onload=alert(1)>", wantErr: true},
		{name: "ftp URL rejected", avatarURL: "ftp://example.com/avatar.png", wantErr: true},
		{name: "javascript: URI rejected", avatarURL: "javascript:alert(1)", wantErr: true},
		{name: "malformed data URI with no comma rejected", avatarURL: "data:image/png;base64", wantErr: true},
		{name: "data URI with unparseable media type rejected", avatarURL: "data:image/png;;;,abc", wantErr: true},
		{name: "oversized data:image/png rejected", avatarURL: "data:image/png;base64," + strings.Repeat("A", maxAvatarDataURILen), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errCode := validateAvatarURL(tc.avatarURL)
			if tc.wantErr && errCode == "" {
				t.Fatalf("validateAvatarURL(%q) = \"\", want a non-empty error code", tc.avatarURL)
			}
			if !tc.wantErr && errCode != "" {
				t.Fatalf("validateAvatarURL(%q) = %q, want \"\"", tc.avatarURL, errCode)
			}
		})
	}
}

func TestValidateAvatarURL_OversizedPayloadGetsSpecificErrorCode(t *testing.T) {
	oversized := "data:image/png;base64," + strings.Repeat("A", maxAvatarDataURILen)
	if got := validateAvatarURL(oversized); got != "avatar_url_too_large" {
		t.Fatalf("validateAvatarURL(oversized) = %q, want \"avatar_url_too_large\"", got)
	}
}

func TestValidateAvatarURL_SVGGetsInvalidFormatErrorCode(t *testing.T) {
	svg := "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="
	if got := validateAvatarURL(svg); got != "invalid_avatar_url_format" {
		t.Fatalf("validateAvatarURL(svg) = %q, want \"invalid_avatar_url_format\"", got)
	}
}

// TestValidateAvatarURL_ReasonablySizedPNGAcceptedExactlyAsBefore documents
// that a normal-sized data:image/png avatar continues to be accepted and
// returned unchanged, matching pre-fix behavior for the common case.
func TestValidateAvatarURL_ReasonablySizedPNGAcceptedExactlyAsBefore(t *testing.T) {
	png := "data:image/png;base64," + strings.Repeat("A", 1024) // 1KB, well under the cap
	if got := validateAvatarURL(png); got != "" {
		t.Fatalf("validateAvatarURL(1KB png) = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// Issue #406: ContributionCalendar() and ContributionActivity() shadowed err
// inside the user_id and own-profile branches, so the github_accounts lookup
// error was discarded. A genuine database failure was served as an empty 200,
// indistinguishable from "this user has no linked GitHub account".
// ---------------------------------------------------------------------------

// scanErrRow is a pgx.Row whose Scan always fails with a fixed error, letting
// a test drive the github_accounts lookup down either the pgx.ErrNoRows path
// or the genuine-failure path without a real database.
type scanErrRow struct{ err error }

func (r scanErrRow) Scan(dest ...any) error { return r.err }

// lookupFailedCode is the error code both handlers report when the
// github_accounts lookup itself fails, as opposed to calendar_fetch_failed /
// activity_fetch_failed further down. Asserting on it proves the 500 came from
// the lookup and not from a later query.
const lookupFailedCode = "github_account_lookup_failed"

// githubLookupPool implements db.DBPool for the contribution handlers: the
// github_accounts lookup (QueryRow) fails with scanErr, while every other
// entry point fails with a distinct error. Nothing panics deliberately — a
// panic inside a Fiber handler makes app.Test return "runtime.Goexit() called
// in handler or server panic" with a nil response and then takes down the whole
// test binary, burying the real assertion. Returning an error instead means a
// handler that wrongly runs the contribution query after a failed lookup
// answers calendar_fetch_failed / activity_fetch_failed, which the error-code
// assertions below catch as an ordinary, readable failure.
type githubLookupPool struct{ scanErr error }

func (p githubLookupPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return scanErrRow{err: p.scanErr}
}
func (p githubLookupPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("contribution query must not run after a failed lookup")
}
func (p githubLookupPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("Exec must not be called by the contribution handlers")
}
func (p githubLookupPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("BeginTx must not be called by the contribution handlers")
}
func (p githubLookupPool) Ping(context.Context) error { return nil }
func (p githubLookupPool) Close()                     {}
func (p githubLookupPool) Config() *pgxpool.Config    { return nil }

// newContributionApp wires both contribution handlers over the given pool.
// jwtSub, when non-empty, is injected as the authenticated caller so the
// own-profile branch can be exercised without real auth middleware.
func newContributionApp(t *testing.T, pool db.DBPool, jwtSub string) *fiber.App {
	t.Helper()
	h := NewUserProfileHandler(config.Config{}, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	inject := func(c *fiber.Ctx) error {
		if jwtSub != "" {
			c.Locals(auth.LocalUserID, jwtSub)
		}
		return c.Next()
	}
	app.Get("/calendar", inject, h.ContributionCalendar())
	app.Get("/activity", inject, h.ContributionActivity())
	return app
}

type contributionBranchCase struct {
	name   string
	path   string
	jwtSub string // empty means "do not authenticate", i.e. use the user_id branch
}

// contributionBranchCases enumerates the four code paths the shadowing bug
// affected: two handlers x {user_id query param, own-profile JWT sub}.
func contributionBranchCases(userID string) []contributionBranchCase {
	return []contributionBranchCase{
		{name: "calendar/user_id branch", path: "/calendar?user_id=" + userID},
		{name: "calendar/own-profile branch", path: "/calendar", jwtSub: userID},
		{name: "activity/user_id branch", path: "/activity?user_id=" + userID},
		{name: "activity/own-profile branch", path: "/activity", jwtSub: userID},
	}
}

// TestContributionHandlers_LookupDBErrorReturns500 is the regression test for
// issue #406: before the fix every one of these cases returned 200 with an
// empty payload, silently swallowing the database error.
func TestContributionHandlers_LookupDBErrorReturns500(t *testing.T) {
	userID := uuid.NewString()
	for _, tc := range contributionBranchCases(userID) {
		t.Run(tc.name, func(t *testing.T) {
			pool := githubLookupPool{scanErr: errors.New("connection reset by peer")}
			app := newContributionApp(t, pool, tc.jwtSub)

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil), -1)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != fiber.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body: %s)", resp.StatusCode, body)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode body %s: %v", body, err)
			}
			// Pins the 500 to the lookup: a handler that instead fell through
			// and ran the contribution query would report
			// calendar_fetch_failed / activity_fetch_failed here.
			if payload["error"] != lookupFailedCode {
				t.Fatalf("error code = %v, want %q (body: %s)", payload["error"], lookupFailedCode, body)
			}
		})
	}
}

// TestContributionHandlers_LookupNoRowsStaysEmpty200 pins the behavior that
// must NOT change: a user with no linked GitHub account still gets an empty
// 200, not the new 500.
func TestContributionHandlers_LookupNoRowsStaysEmpty200(t *testing.T) {
	userID := uuid.NewString()
	for _, tc := range contributionBranchCases(userID) {
		t.Run(tc.name, func(t *testing.T) {
			app := newContributionApp(t, githubLookupPool{scanErr: pgx.ErrNoRows}, tc.jwtSub)

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tc.path, nil), -1)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
			}

			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode body %s: %v", body, err)
			}
			if total, ok := payload["total"].(float64); !ok || total != 0 {
				t.Fatalf("total = %v, want 0 (body: %s)", payload["total"], body)
			}
		})
	}
}
