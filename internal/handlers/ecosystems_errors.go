package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
)

func ecosystemLookupFailure(err error) (int, string) {
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.StatusNotFound, "ecosystem_not_found"
	}
	return fiber.StatusInternalServerError, "ecosystem_lookup_failed"
}
