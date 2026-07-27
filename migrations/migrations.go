package migrations

import (
	"embed"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
)

// FS contains all migration SQL files.
//
// Note: embed patterns cannot use "..", so the embedding must live alongside the SQL files.
//go:embed *
var FS embed.FS






















// ComputeChecksum returns the SHA-256 checksum of the up migration file for the given version.
func ComputeChecksum(version uint) (string, error) {
	pattern := fmt.Sprintf("%06d_*.up.sql", version)
	matches, err := fs.Glob(FS, pattern)
	if err != nil {
		return "", fmt.Errorf("glob pattern error: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no up migration file found for version %d", version)
	}
	content, err := fs.ReadFile(FS, matches[0])
	if err != nil {
		return "", fmt.Errorf("read migration file: %w", err)
	}
	h := sha256.New()
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)), nil
}
