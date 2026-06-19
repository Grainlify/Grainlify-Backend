package handlers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

func TestEcosystemLookupFailureMapsNoRowsToNotFound(t *testing.T) {
	status, code := ecosystemLookupFailure(pgx.ErrNoRows)

	if status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, fiber.StatusNotFound)
	}
	if code != "ecosystem_not_found" {
		t.Fatalf("code = %q, want %q", code, "ecosystem_not_found")
	}
}

func TestEcosystemLookupFailureMapsWrappedNoRowsToNotFound(t *testing.T) {
	status, code := ecosystemLookupFailure(fmt.Errorf("lookup ecosystem: %w", pgx.ErrNoRows))

	if status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, fiber.StatusNotFound)
	}
	if code != "ecosystem_not_found" {
		t.Fatalf("code = %q, want %q", code, "ecosystem_not_found")
	}
}

func TestEcosystemLookupFailureMapsOtherErrorsToInternalServerError(t *testing.T) {
	status, code := ecosystemLookupFailure(errors.New("database connection reset"))

	if status != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status, fiber.StatusInternalServerError)
	}
	if code != "ecosystem_lookup_failed" {
		t.Fatalf("code = %q, want %q", code, "ecosystem_lookup_failed")
	}
}
