package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"
)

// ErrPaginationResponded is returned by ParsePagination when it has already
// written an error response to the client (e.g. a 400 for a negative offset).
// Callers should stop processing the request when they receive a non-nil
// error and simply return nil, since the response is already committed.
var ErrPaginationResponded = errors.New("pagination: response already written")

// PaginationParams holds validated pagination parameters extracted from
// request query strings. Limit is clamped to [1, maxLimit] and offset is
// guaranteed to be non-negative.
type PaginationParams struct {
	Limit  int
	Offset int
}

// ParsePagination extracts limit and offset from the request query string.
//
// It applies the following rules:
//   - If limit is missing, zero, or negative, defaultLimit is used.
//   - If limit exceeds maxLimit, it is clamped to maxLimit.
//   - If offset is negative, a 400 Bad Request is returned.
func ParsePagination(c *fiber.Ctx, defaultLimit, maxLimit int) (PaginationParams, error) {
	limit := c.QueryInt("limit", defaultLimit)
	if limit < 1 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	offset := c.QueryInt("offset", 0)
	if offset < 0 {
		// Write the 400 response now and signal callers (via the non-nil
		// sentinel) to stop processing. Because the body is already
		// committed, callers must return nil rather than the error so the
		// framework's error handler does not overwrite this response.
		_ = c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "offset must be non-negative",
		})
		return PaginationParams{}, ErrPaginationResponded
	}

	return PaginationParams{Limit: limit, Offset: offset}, nil
}

// PaginatedResponse builds a paginated response envelope.
//
// The returned map has the shape:
//
//	{ "<itemsKey>": [...], "limit": N, "offset": N, "total": N, "has_more": bool }
//
// If items is nil, an empty slice is substituted so the JSON always contains
// an array value for the items key.
func PaginatedResponse(itemsKey string, items any, p PaginationParams, total int) fiber.Map {
	if items == nil {
		items = []any{}
	}
	return fiber.Map{
		itemsKey:   items,
		"limit":    p.Limit,
		"offset":   p.Offset,
		"total":    total,
		"has_more": p.Offset+p.Limit < total,
	}
}
