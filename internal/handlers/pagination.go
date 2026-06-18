package handlers

import (
	"github.com/gofiber/fiber/v2"
)

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
		return PaginationParams{}, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "offset must be non-negative",
		})
	}

	return PaginationParams{Limit: limit, Offset: offset}, nil
}

// PaginatedResponse builds a paginated response envelope.
//
// The returned map has the shape:
//
//	{ "<itemsKey>": [...], "limit": N, "offset": N, "total": N }
//
// If items is nil, an empty slice is substituted so the JSON always contains
// an array value for the items key.
func PaginatedResponse(itemsKey string, items any, p PaginationParams, total int) fiber.Map {
	if items == nil {
		items = []any{}
	}
	return fiber.Map{
		itemsKey: items,
		"limit":  p.Limit,
		"offset": p.Offset,
		"total":  total,
	}
}
