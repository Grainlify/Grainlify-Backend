package api

import (
	"strings"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// CORSOriginPolicy encapsulates origin-matching rules for credentialed CORS.
// With AllowCredentials enabled, this policy is the only guard against cross-site abuse.
type CORSOriginPolicy struct {
	allowDevLocalhost     bool
	allowPreviewWildcards bool
	explicitOrigins       map[string]struct{}
	frontendBaseURL       string
}

// BuildCORSOriginPolicy derives CORS matching rules from application config.
func BuildCORSOriginPolicy(cfg config.Config) CORSOriginPolicy {
	explicitOrigins := map[string]struct{}{}
	if strings.TrimSpace(cfg.CORSOrigins) != "" {
		for _, origin := range strings.Split(cfg.CORSOrigins, ",") {
			origin = normalizeOrigin(origin)
			if origin == "" {
				continue
			}
			explicitOrigins[origin] = struct{}{}
		}
	}

	return CORSOriginPolicy{
		allowDevLocalhost:     cfg.IsDev(),
		allowPreviewWildcards: cfg.CORSAllowPreview,
		explicitOrigins:       explicitOrigins,
		frontendBaseURL:       strings.TrimSuffix(strings.TrimSpace(cfg.FrontendBaseURL), "/"),
	}
}

// Allows reports whether the given Origin header value may receive credentialed CORS responses.
func (p CORSOriginPolicy) Allows(origin string) bool {
	origin = normalizeOrigin(origin)
	if origin == "" {
		return false
	}

	if p.allowDevLocalhost && isLocalhostOrigin(origin) {
		return true
	}

	if p.allowPreviewWildcards {
		if strings.HasSuffix(origin, ".vercel.app") || strings.HasSuffix(origin, ".0xo.in") {
			return true
		}
	}

	if _, ok := p.explicitOrigins[origin]; ok {
		return true
	}

	if p.frontendBaseURL != "" && origin == p.frontendBaseURL {
		return true
	}

	return false
}

func normalizeOrigin(origin string) string {
	return strings.TrimSuffix(strings.TrimSpace(origin), "/")
}

func isLocalhostOrigin(origin string) bool {
	return strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://localhost:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:")
}
