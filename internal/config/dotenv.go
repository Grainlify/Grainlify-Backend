package config

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// LoadDotenv loads environment variables from a local .env file if present.
//
// It does NOT override already-exported environment variables.
// This is meant for local development convenience.
func LoadDotenv() {
	// Allow an explicit env file path (or comma-separated list).
	if v := strings.TrimSpace(os.Getenv("ENV_FILE")); v != "" {
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
			loadEnvFile(parts[i])
		}
		return
	}

	// Default: try a few common locations, so starting the process from repo root or backend/ works.
	// We keep it silent and best-effort for missing files, but warn on malformed content.
	candidates := []string{".env"}

	if wd, err := os.Getwd(); err == nil {
		// If running from repo root.
		candidates = append(candidates, filepath.Join(wd, "backend", ".env"))
		// If running from backend/ already or from backend/cmd.
		candidates = append(candidates, filepath.Join(wd, ".env"))
		candidates = append(candidates, filepath.Join(wd, "..", "backend", ".env"))
	}

	for _, p := range candidates {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			loadEnvFile(p)
			return
		}
	}
}

func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var buf strings.Builder
	envMap := make(map[string]string)

	for scanner.Scan() {
		line := scanner.Text()

		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(line)

		current := buf.String()

		// Skip empty lines and pure comments
		if strings.TrimSpace(current) == "" || strings.HasPrefix(strings.TrimSpace(current), "#") {
			buf.Reset()
			continue
		}

		parsedMap, err := godotenv.Unmarshal(current)
		if err == nil {
			for k, v := range parsedMap {
				if k == "" {
					log.Printf("WARNING: malformed line in %s (missing '='): %q\n", path, current)
				} else {
					envMap[k] = v
				}
			}
			buf.Reset()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("WARNING: error reading %s: %v\n", path, err)
	} else if buf.Len() > 0 {
		log.Printf("WARNING: malformed or unterminated line in %s: %q\n", path, buf.String())
	}

	for k, v := range envMap {
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}
