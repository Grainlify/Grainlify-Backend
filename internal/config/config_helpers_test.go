package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLogLevel(t *testing.T) {
	tests := []struct {
		envVal   string
		expected slog.Leveler
	}{
		{"debug", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"invalid", slog.LevelInfo},
		{"-4", slog.Level(-4)},
		{"4", slog.Level(4)},
	}

	for _, tc := range tests {
		cfg := Config{Log: tc.envVal}
		if lvl := cfg.LogLevel(); lvl != tc.expected {
			t.Errorf("for Log %q, expected %v, got %v", tc.envVal, tc.expected, lvl)
		}
	}
}

func TestGetEnvHelpers(t *testing.T) {
	t.Run("getEnv", func(t *testing.T) {
		t.Setenv("TEST_GET_ENV", "  value  ")
		if got := getEnv("TEST_GET_ENV", "default"); got != "  value  " {
			t.Errorf("expected '  value  ', got %q", got)
		}
		
		t.Setenv("TEST_GET_ENV_EMPTY", "   ")
		if got := getEnv("TEST_GET_ENV_EMPTY", "default"); got != "default" {
			t.Errorf("expected 'default', got %q", got)
		}
	})

	t.Run("getEnvInt32", func(t *testing.T) {
		t.Setenv("TEST_GET_ENV_INT32", "42")
		if got := getEnvInt32("TEST_GET_ENV_INT32", 10); got != 42 {
			t.Errorf("expected 42, got %d", got)
		}

		t.Setenv("TEST_GET_ENV_INT32_BAD", "abc")
		if got := getEnvInt32("TEST_GET_ENV_INT32_BAD", 10); got != 10 {
			t.Errorf("expected 10, got %d", got)
		}
		
		t.Setenv("TEST_GET_ENV_INT32_NEG", "-5")
		if got := getEnvInt32("TEST_GET_ENV_INT32_NEG", 10); got != 10 {
			t.Errorf("expected 10 (negative fallback), got %d", got)
		}

		t.Setenv("TEST_GET_ENV_INT32_EMPTY", "   ")
		if got := getEnvInt32("TEST_GET_ENV_INT32_EMPTY", 10); got != 10 {
			t.Errorf("expected 10, got %d", got)
		}
	})

	t.Run("getEnvInt", func(t *testing.T) {
		t.Setenv("TEST_GET_ENV_INT", "100")
		if got := getEnvInt("TEST_GET_ENV_INT", 50); got != 100 {
			t.Errorf("expected 100, got %d", got)
		}

		t.Setenv("TEST_GET_ENV_INT_BAD", "xyz")
		if got := getEnvInt("TEST_GET_ENV_INT_BAD", 50); got != 50 {
			t.Errorf("expected 50, got %d", got)
		}

		t.Setenv("TEST_GET_ENV_INT_NEG", "-1")
		if got := getEnvInt("TEST_GET_ENV_INT_NEG", 50); got != 50 {
			t.Errorf("expected 50, got %d", got)
		}
		
		t.Setenv("TEST_GET_ENV_INT_EMPTY", "")
		if got := getEnvInt("TEST_GET_ENV_INT_EMPTY", 50); got != 50 {
			t.Errorf("expected 50, got %d", got)
		}
	})

	t.Run("getEnvDuration", func(t *testing.T) {
		t.Setenv("TEST_GET_ENV_DUR", "10s")
		if got := getEnvDuration("TEST_GET_ENV_DUR", time.Minute); got != 10*time.Second {
			t.Errorf("expected 10s, got %v", got)
		}

		t.Setenv("TEST_GET_ENV_DUR_BAD", "not-a-dur")
		if got := getEnvDuration("TEST_GET_ENV_DUR_BAD", time.Minute); got != time.Minute {
			t.Errorf("expected 1m, got %v", got)
		}

		t.Setenv("TEST_GET_ENV_DUR_NEG", "-10s")
		if got := getEnvDuration("TEST_GET_ENV_DUR_NEG", time.Minute); got != time.Minute {
			t.Errorf("expected 1m, got %v", got)
		}
		
		t.Setenv("TEST_GET_ENV_DUR_EMPTY", "")
		if got := getEnvDuration("TEST_GET_ENV_DUR_EMPTY", time.Minute); got != time.Minute {
			t.Errorf("expected 1m, got %v", got)
		}
	})

	t.Run("getEnvBool", func(t *testing.T) {
		trues := []string{"1", "true", "t", "yes", "y", "on"}
		for _, v := range trues {
			t.Setenv("TEST_GET_ENV_BOOL", v)
			if !getEnvBool("TEST_GET_ENV_BOOL", false) {
				t.Errorf("expected true for %q", v)
			}
		}

		falses := []string{"0", "false", "f", "no", "n", "off"}
		for _, v := range falses {
			t.Setenv("TEST_GET_ENV_BOOL", v)
			if getEnvBool("TEST_GET_ENV_BOOL", true) {
				t.Errorf("expected false for %q", v)
			}
		}

		t.Setenv("TEST_GET_ENV_BOOL", "invalid")
		if got := getEnvBool("TEST_GET_ENV_BOOL", true); got != true {
			t.Errorf("expected fallback true, got %v", got)
		}
		
		t.Setenv("TEST_GET_ENV_BOOL_EMPTY", "   ")
		if got := getEnvBool("TEST_GET_ENV_BOOL_EMPTY", true); got != true {
			t.Errorf("expected fallback true, got %v", got)
		}
	})
}
