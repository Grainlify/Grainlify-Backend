package handlers_test

import (
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/stretchr/testify/assert"
)

func TestAuthRoutesRegistered(t *testing.T) {
	cfg := config.Config{JWTSecret: "secret"}
	// Minimal setup
	app := api.New(cfg, api.Deps{DB: &db.DB{}})

	routes := app.GetRoutes()
	hasNonce := false
	hasVerify := false
	for _, r := range routes {
		if r.Method == "POST" && r.Path == "/auth/nonce" {
			hasNonce = true
		}
		if r.Method == "POST" && r.Path == "/auth/verify" {
			hasVerify = true
		}
	}
	assert.True(t, hasNonce, "POST /auth/nonce route should be registered")
	assert.True(t, hasVerify, "POST /auth/verify route should be registered")
}
