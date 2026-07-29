package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

// urlOfLen builds a syntactically valid absolute URL of exactly totalLen bytes.
func urlOfLen(totalLen int) string {
	const prefix = "https://example.com/"
	if totalLen <= len(prefix) {
		return prefix
	}
	return prefix + strings.Repeat("a", totalLen-len(prefix))
}

// rawJSONString builds a valid JSON string literal whose raw encoding is
// exactly totalLen bytes (including the surrounding quotes).
func rawJSONString(totalLen int) json.RawMessage {
	if totalLen < 2 {
		totalLen = 2
	}
	return json.RawMessage(`"` + strings.Repeat("a", totalLen-2) + `"`)
}

// ---- validateEcosystemInput unit tests ----

func TestValidateEcosystemInput(t *testing.T) {
	validName := "Valid Ecosystem"

	tests := []struct {
		name     string
		req      ecosystemUpsertRequest
		isUpdate bool
		want     string // expected error code; "" means validation passes
	}{
		{
			name: "valid minimal create",
			req:  ecosystemUpsertRequest{Name: validName},
			want: "",
		},
		{
			name: "name required on create",
			req:  ecosystemUpsertRequest{Name: ""},
			want: "name_required",
		},
		{
			name:     "empty name allowed on update",
			req:      ecosystemUpsertRequest{Name: ""},
			isUpdate: true,
			want:     "",
		},
		{
			name: "invalid slug from name with no valid characters",
			req:  ecosystemUpsertRequest{Name: "!!!"},
			want: "invalid_slug",
		},
		{
			name: "name exactly at max length",
			req:  ecosystemUpsertRequest{Name: strings.Repeat("a", maxEcosystemNameLen)},
			want: "",
		},
		{
			name: "name too long",
			req:  ecosystemUpsertRequest{Name: strings.Repeat("a", maxEcosystemNameLen+1)},
			want: "name_too_long",
		},
		{
			name:     "name too long on update",
			req:      ecosystemUpsertRequest{Name: strings.Repeat("a", maxEcosystemNameLen+1)},
			isUpdate: true,
			want:     "name_too_long",
		},
		{
			name: "description exactly at max length",
			req:  ecosystemUpsertRequest{Name: validName, Description: strings.Repeat("a", maxEcosystemDescriptionLen)},
			want: "",
		},
		{
			name: "description too long",
			req:  ecosystemUpsertRequest{Name: validName, Description: strings.Repeat("a", maxEcosystemDescriptionLen+1)},
			want: "description_too_long",
		},
		{
			name: "description length ignores surrounding whitespace",
			req:  ecosystemUpsertRequest{Name: validName, Description: "  " + strings.Repeat("a", maxEcosystemDescriptionLen) + "  "},
			want: "",
		},
		{
			name: "about exactly at max length",
			req:  ecosystemUpsertRequest{Name: validName, About: strings.Repeat("a", maxEcosystemAboutLen)},
			want: "",
		},
		{
			name: "about too long",
			req:  ecosystemUpsertRequest{Name: validName, About: strings.Repeat("a", maxEcosystemAboutLen+1)},
			want: "about_too_long",
		},
		{
			name: "website_url exactly at max length",
			req:  ecosystemUpsertRequest{Name: validName, WebsiteURL: urlOfLen(maxEcosystemURLLen)},
			want: "",
		},
		{
			name: "website_url too long",
			req:  ecosystemUpsertRequest{Name: validName, WebsiteURL: urlOfLen(maxEcosystemURLLen + 1)},
			want: "website_url_too_long",
		},
		{
			name: "website_url invalid format within length limit",
			req:  ecosystemUpsertRequest{Name: validName, WebsiteURL: "not a url"},
			want: "website_url_invalid",
		},
		{
			name: "logo_url exactly at max length",
			req:  ecosystemUpsertRequest{Name: validName, LogoURL: urlOfLen(maxEcosystemURLLen)},
			want: "",
		},
		{
			name: "logo_url too long",
			req:  ecosystemUpsertRequest{Name: validName, LogoURL: urlOfLen(maxEcosystemURLLen + 1)},
			want: "logo_url_too_long",
		},
		{
			name: "logo_url invalid format within length limit",
			req:  ecosystemUpsertRequest{Name: validName, LogoURL: "not a url"},
			want: "logo_url_invalid",
		},
		{
			name: "links exactly at max size",
			req:  ecosystemUpsertRequest{Name: validName, Links: rawJSONString(maxEcosystemJSONFieldBytes)},
			want: "",
		},
		{
			name: "links too large",
			req:  ecosystemUpsertRequest{Name: validName, Links: rawJSONString(maxEcosystemJSONFieldBytes + 1)},
			want: "links_too_long",
		},
		{
			name: "key_areas exactly at max size",
			req:  ecosystemUpsertRequest{Name: validName, KeyAreas: rawJSONString(maxEcosystemJSONFieldBytes)},
			want: "",
		},
		{
			name: "key_areas too large",
			req:  ecosystemUpsertRequest{Name: validName, KeyAreas: rawJSONString(maxEcosystemJSONFieldBytes + 1)},
			want: "key_areas_too_long",
		},
		{
			name: "technologies exactly at max size",
			req:  ecosystemUpsertRequest{Name: validName, Technologies: rawJSONString(maxEcosystemJSONFieldBytes)},
			want: "",
		},
		{
			name: "technologies too large",
			req:  ecosystemUpsertRequest{Name: validName, Technologies: rawJSONString(maxEcosystemJSONFieldBytes + 1)},
			want: "technologies_too_long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			if err := validateEcosystemInput(&tt.req, tt.isUpdate); err != nil {
				got = err.Error()
			}
			if got != tt.want {
				t.Errorf("validateEcosystemInput() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- Handler-level fakes ----

// ecosystemPanicPool fails loudly if the handler reaches the database before
// validation has run: every method panics except the always-safe Ping,
// Close and Config.
type ecosystemPanicPool struct{}

func (p *ecosystemPanicPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec call: validation should have short-circuited")
}
func (p *ecosystemPanicPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query call: validation should have short-circuited")
}
func (p *ecosystemPanicPool) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow call: validation should have short-circuited")
}
func (p *ecosystemPanicPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("unexpected BeginTx call: validation should have short-circuited")
}
func (p *ecosystemPanicPool) Ping(context.Context) error { return nil }
func (p *ecosystemPanicPool) Close()                     {}
func (p *ecosystemPanicPool) Config() *pgxpool.Config    { return nil }

// ecosystemFakePool is a working stand-in DB for the happy path: slug
// existence checks report existsVal, inserts report insertID, and updates
// report execTag/execErr.
type ecosystemFakePool struct {
	existsVal bool
	insertID  uuid.UUID
	execTag   pgconn.CommandTag
	execErr   error
}

func (p *ecosystemFakePool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return p.execTag, p.execErr
}
func (p *ecosystemFakePool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("not implemented")
}
func (p *ecosystemFakePool) QueryRow(context.Context, string, ...any) pgx.Row {
	return ecosystemFakeRow{exists: p.existsVal, id: p.insertID}
}
func (p *ecosystemFakePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("not implemented")
}
func (p *ecosystemFakePool) Ping(context.Context) error { return nil }
func (p *ecosystemFakePool) Close()                     {}
func (p *ecosystemFakePool) Config() *pgxpool.Config    { return nil }

type ecosystemFakeRow struct {
	exists bool
	id     uuid.UUID
}

func (r ecosystemFakeRow) Scan(dest ...any) error {
	for _, d := range dest {
		switch v := d.(type) {
		case *bool:
			*v = r.exists
		case *uuid.UUID:
			*v = r.id
		}
	}
	return nil
}

func ecosystemCreateApp(pool db.DBPool) *fiber.App {
	h := NewEcosystemsAdminHandler(&db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/admin/ecosystems", h.Create())
	return app
}

func ecosystemUpdateApp(pool db.DBPool) *fiber.App {
	h := NewEcosystemsAdminHandler(&db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Put("/admin/ecosystems/:id", h.Update())
	return app
}

func postEcosystem(t *testing.T, app *fiber.App, req ecosystemUpsertRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/admin/ecosystems", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(httpReq)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

func putEcosystem(t *testing.T, app *fiber.App, id string, req ecosystemUpsertRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest(http.MethodPut, "/admin/ecosystems/"+id, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(httpReq)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

func decodeErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var env httpx.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error
}

func validEcosystemCreateReq() ecosystemUpsertRequest {
	return ecosystemUpsertRequest{
		Name:   "Valid Ecosystem",
		Status: "active",
	}
}

// ---- Field length limit matrix: over-limit and boundary, for Create and Update ----

type ecosystemFieldLimitCase struct {
	fieldName   string
	setOver     func(*ecosystemUpsertRequest)
	setBoundary func(*ecosystemUpsertRequest)
	wantCode    string
}

var ecosystemFieldLimitCases = []ecosystemFieldLimitCase{
	{
		fieldName:   "name",
		setOver:     func(r *ecosystemUpsertRequest) { r.Name = strings.Repeat("a", maxEcosystemNameLen+1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.Name = strings.Repeat("a", maxEcosystemNameLen) },
		wantCode:    "name_too_long",
	},
	{
		fieldName:   "description",
		setOver:     func(r *ecosystemUpsertRequest) { r.Description = strings.Repeat("a", maxEcosystemDescriptionLen+1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.Description = strings.Repeat("a", maxEcosystemDescriptionLen) },
		wantCode:    "description_too_long",
	},
	{
		fieldName:   "about",
		setOver:     func(r *ecosystemUpsertRequest) { r.About = strings.Repeat("a", maxEcosystemAboutLen+1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.About = strings.Repeat("a", maxEcosystemAboutLen) },
		wantCode:    "about_too_long",
	},
	{
		fieldName:   "website_url",
		setOver:     func(r *ecosystemUpsertRequest) { r.WebsiteURL = urlOfLen(maxEcosystemURLLen + 1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.WebsiteURL = urlOfLen(maxEcosystemURLLen) },
		wantCode:    "website_url_too_long",
	},
	{
		fieldName:   "logo_url",
		setOver:     func(r *ecosystemUpsertRequest) { r.LogoURL = urlOfLen(maxEcosystemURLLen + 1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.LogoURL = urlOfLen(maxEcosystemURLLen) },
		wantCode:    "logo_url_too_long",
	},
	{
		fieldName:   "links",
		setOver:     func(r *ecosystemUpsertRequest) { r.Links = rawJSONString(maxEcosystemJSONFieldBytes + 1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.Links = rawJSONString(maxEcosystemJSONFieldBytes) },
		wantCode:    "links_too_long",
	},
	{
		fieldName:   "key_areas",
		setOver:     func(r *ecosystemUpsertRequest) { r.KeyAreas = rawJSONString(maxEcosystemJSONFieldBytes + 1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.KeyAreas = rawJSONString(maxEcosystemJSONFieldBytes) },
		wantCode:    "key_areas_too_long",
	},
	{
		fieldName:   "technologies",
		setOver:     func(r *ecosystemUpsertRequest) { r.Technologies = rawJSONString(maxEcosystemJSONFieldBytes + 1) },
		setBoundary: func(r *ecosystemUpsertRequest) { r.Technologies = rawJSONString(maxEcosystemJSONFieldBytes) },
		wantCode:    "technologies_too_long",
	},
}

func TestCreate_FieldLengthLimits(t *testing.T) {
	for _, tc := range ecosystemFieldLimitCases {
		tc := tc

		t.Run(tc.fieldName+"/over_limit_rejected", func(t *testing.T) {
			req := validEcosystemCreateReq()
			tc.setOver(&req)

			// A panicking pool proves validation rejects the request before
			// any database call is made.
			app := ecosystemCreateApp(&ecosystemPanicPool{})
			resp := postEcosystem(t, app, req)

			if resp.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
			}
			if code := decodeErrorCode(t, resp); code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", code, tc.wantCode)
			}
		})

		t.Run(tc.fieldName+"/at_boundary_accepted", func(t *testing.T) {
			req := validEcosystemCreateReq()
			tc.setBoundary(&req)

			pool := &ecosystemFakePool{insertID: uuid.New()}
			app := ecosystemCreateApp(pool)
			resp := postEcosystem(t, app, req)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusCreated {
				t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusCreated)
			}
		})
	}
}

func TestUpdate_FieldLengthLimits(t *testing.T) {
	for _, tc := range ecosystemFieldLimitCases {
		tc := tc

		t.Run(tc.fieldName+"/over_limit_rejected", func(t *testing.T) {
			var req ecosystemUpsertRequest
			tc.setOver(&req)

			app := ecosystemUpdateApp(&ecosystemPanicPool{})
			resp := putEcosystem(t, app, uuid.New().String(), req)

			if resp.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
			}
			if code := decodeErrorCode(t, resp); code != tc.wantCode {
				t.Fatalf("error code = %q, want %q", code, tc.wantCode)
			}
		})

		t.Run(tc.fieldName+"/at_boundary_accepted", func(t *testing.T) {
			var req ecosystemUpsertRequest
			tc.setBoundary(&req)

			pool := &ecosystemFakePool{execTag: pgconn.NewCommandTag("UPDATE 1")}
			app := ecosystemUpdateApp(pool)
			resp := putEcosystem(t, app, uuid.New().String(), req)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
			}
		})
	}
}

func TestUpdate_InvalidJSONStillRejectedBeforeValidation(t *testing.T) {
	app := ecosystemUpdateApp(&ecosystemPanicPool{})
	req := httptest.NewRequest(http.MethodPut, "/admin/ecosystems/"+uuid.New().String(), strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusBadRequest)
	}
}
