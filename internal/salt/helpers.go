package salt

import (
	"strings"

	"github.com/jackc/pgx/v5"
)

func trimSpace(s string) string { return strings.TrimSpace(s) }

func isNoRows(err error) bool {
	return err == pgx.ErrNoRows || strings.Contains(err.Error(), "no rows")
}
