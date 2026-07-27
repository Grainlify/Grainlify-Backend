package api

import (
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/stretchr/testify/assert"
)

func prodConfig() config.Config {
	return config.Config{
		Env:             "production",
		CORSOrigins:     "https://grainlify.0xo.in,https://api.grainlify.0xo.in",
		FrontendBaseURL: "https://grainlify.0xo.in",
		CORSAllowPreview: false,
	}
}

func devConfig() config.Config {
	return config.Config{
		Env:             "dev",
		CORSOrigins:     "http://localhost:5173",
		FrontendBaseURL: "http://localhost:5173",
		CORSAllowPreview: false,
	}
}

func TestCORSOriginPolicy_ProductionDeniesWildcardsByDefault(t *testing.T) {
	policy := BuildCORSOriginPolicy(prodConfig())

	denied := []string{
		"https://attacker.vercel.app",
		"https://evil-preview.vercel.app",
		"https://grainlify.0xo.in.evil.com",
		"https://fake.0xo.in.attacker.net",
		"http://localhost:5173",
		"https://127.0.0.1:5173",
		"https://not-in-allowlist.example.com",
	}
	for _, origin := range denied {
		assert.False(t, policy.Allows(origin), "origin %q should be denied in production", origin)
	}
}

func TestCORSOriginPolicy_ProductionAllowsExplicitAndFrontendOrigins(t *testing.T) {
	policy := BuildCORSOriginPolicy(prodConfig())

	allowed := []string{
		"https://grainlify.0xo.in",
		"https://api.grainlify.0xo.in",
		"https://grainlify.0xo.in/",
	}
	for _, origin := range allowed {
		assert.True(t, policy.Allows(origin), "origin %q should be allowed in production", origin)
	}
}

func TestCORSOriginPolicy_DevAllowsLocalhostOnly(t *testing.T) {
	policy := BuildCORSOriginPolicy(devConfig())

	assert.True(t, policy.Allows("http://localhost:5173"))
	assert.True(t, policy.Allows("http://127.0.0.1:3000"))
	assert.True(t, policy.Allows("https://localhost:4173"))
	assert.False(t, policy.Allows("https://preview.vercel.app"))
	assert.False(t, policy.Allows("https://grainlify.0xo.in"))
}

func TestCORSOriginPolicy_PreviewWildcardsGatedByConfig(t *testing.T) {
	cfg := prodConfig()
	cfg.CORSAllowPreview = true
	policy := BuildCORSOriginPolicy(cfg)

	assert.True(t, policy.Allows("https://grainlify-git-feature.vercel.app"))
	assert.True(t, policy.Allows("https://api.grainlify.0xo.in"))
	assert.False(t, policy.Allows("https://evil.vercel.app.attacker.com"))
	assert.False(t, policy.Allows("https://notvercel.app"))
	assert.False(t, policy.Allows("https://0xo.in"))
}

func TestCORSOriginPolicy_SubdomainSpoofAttemptsDenied(t *testing.T) {
	cfg := prodConfig()
	cfg.CORSAllowPreview = true
	policy := BuildCORSOriginPolicy(cfg)

	spoofs := []string{
		"https://evil.vercel.app.attacker.com",
		"https://grainlify.0xo.in.evil.com",
		"https://fake-0xo.in",
		"https://vercel.app",
		"https://0xo.in",
		"https://malicious.localhost:5173.evil.com",
	}
	for _, origin := range spoofs {
		assert.False(t, policy.Allows(origin), "spoof origin %q must be denied", origin)
	}
}

func TestCORSOriginPolicy_EmptyOriginDenied(t *testing.T) {
	policy := BuildCORSOriginPolicy(prodConfig())
	assert.False(t, policy.Allows(""))
	assert.False(t, policy.Allows("   "))
}

func TestCORSOriginPolicy_TrailingSlashNormalized(t *testing.T) {
	policy := BuildCORSOriginPolicy(config.Config{
		Env:         "production",
		CORSOrigins: "https://grainlify.0xo.in/",
	})

	assert.True(t, policy.Allows("https://grainlify.0xo.in"))
	assert.True(t, policy.Allows("https://grainlify.0xo.in/"))
}

func TestBuildCORSOriginPolicy_ParsesCommaSeparatedOrigins(t *testing.T) {
	policy := BuildCORSOriginPolicy(config.Config{
		Env:         "production",
		CORSOrigins: " https://a.example.com , https://b.example.com ",
	})

	assert.True(t, policy.Allows("https://a.example.com"))
	assert.True(t, policy.Allows("https://b.example.com"))
	assert.False(t, policy.Allows("https://c.example.com"))
}

func TestIsLocalhostOrigin(t *testing.T) {
	assert.True(t, isLocalhostOrigin("http://localhost:5173"))
	assert.True(t, isLocalhostOrigin("https://127.0.0.1:8080"))
	assert.False(t, isLocalhostOrigin("https://localhost.evil.com:5173"))
	assert.False(t, isLocalhostOrigin("https://example.com"))
}
