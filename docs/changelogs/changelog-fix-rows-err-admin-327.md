## Changelog - Fix rows.Err() handling in admin.go (Issue #327)

### Description
Resolved an issue where database row iteration in `AdminHandler.ListUsers` lacked `rows.Err()` checks, potentially leading to silent data truncation or incomplete responses if an error occurred during iteration.

### Changes
- Modified `internal/handlers/admin.go` to include `rows.Err()` verification after the `rows.Next()` loop.
- Added a validation test file `internal/handlers/admin_rows_err_test.go` to ensure code integrity.

### Verification Steps
- Performed manual logic verification of `rows.Err()` implementation.
- Confirmed compilation integrity with `go build ./...`.
- Verified handlers maintain expected behavior.
