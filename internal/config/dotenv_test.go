package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotenv_Cases(t *testing.T) {
	// Capture logs to verify warnings on malformed lines
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")

	// Helper to write file and call LoadDotenv via ENV_FILE
	testFile := func(content string) {
		buf.Reset()
		err := os.WriteFile(envFile, []byte(content), 0644)
		if err != nil {
			t.Fatalf("failed to write env file: %v", err)
		}
		t.Setenv("ENV_FILE", envFile)
		LoadDotenv()
	}

	t.Run("empty file", func(t *testing.T) {
		testFile("")
		if buf.Len() > 0 {
			t.Errorf("expected no warning for empty file, got: %s", buf.String())
		}
	})

	t.Run("comment only", func(t *testing.T) {
		testFile("# this is a comment\n# another comment\n")
		if buf.Len() > 0 {
			t.Errorf("expected no warning for comment-only file, got: %s", buf.String())
		}
	})

	t.Run("duplicate key", func(t *testing.T) {
		os.Unsetenv("TEST_DUP_KEY")
		testFile("TEST_DUP_KEY=first\nTEST_DUP_KEY=second\n")
		if buf.Len() > 0 {
			t.Errorf("expected no warning for duplicate key, got: %s", buf.String())
		}
		// godotenv parses into a map, so last defined key wins
		if val := os.Getenv("TEST_DUP_KEY"); val != "second" {
			t.Errorf("expected 'second' (last-wins) for duplicate key, got: %q", val)
		}
		os.Unsetenv("TEST_DUP_KEY")
	})

	t.Run("missing equals", func(t *testing.T) {
		testFile("VALID=1\nMISSING_EQUALS\n")
		out := buf.String()
		if out == "" {
			t.Error("expected warning for missing equals sign")
		} else if !strings.Contains(out, "Warning: failed to load ENV_FILE") {
			t.Errorf("unexpected warning format: %s", out)
		}
	})

	t.Run("unterminated quote", func(t *testing.T) {
		testFile("VALID=1\nUNTERM=\"unterminated\n")
		out := buf.String()
		if out == "" {
			t.Error("expected warning for unterminated quote")
		} else if !strings.Contains(out, "Warning: failed to load ENV_FILE") {
			t.Errorf("unexpected warning format: %s", out)
		}
	})
	
	t.Run("windows line endings", func(t *testing.T) {
		os.Unsetenv("TEST_WIN_CRLF")
		testFile("TEST_WIN_CRLF=123\r\n")
		if buf.Len() > 0 {
			t.Errorf("expected no warning for CRLF file, got: %s", buf.String())
		}
		if val := os.Getenv("TEST_WIN_CRLF"); val != "123" {
			t.Errorf("expected '123' for CRLF, got: %q", val)
		}
		os.Unsetenv("TEST_WIN_CRLF")
	})

	t.Run("default locations fallback", func(t *testing.T) {
		t.Setenv("ENV_FILE", "") // Ensure explicit file is empty to trigger fallback
		os.Unsetenv("TEST_DEFAULT_LOC")
		
		cwd, _ := os.Getwd()
		defaultEnvFile := filepath.Join(cwd, ".env")
		
		// Create a temporary .env in the current directory
		err := os.WriteFile(defaultEnvFile, []byte("TEST_DEFAULT_LOC=fallback_val"), 0644)
		if err != nil {
			t.Fatalf("failed to write default env file: %v", err)
		}
		defer os.Remove(defaultEnvFile)
		
		LoadDotenv()
		
		if val := os.Getenv("TEST_DEFAULT_LOC"); val != "fallback_val" {
			t.Errorf("expected 'fallback_val' from default location, got: %q", val)
		}
		os.Unsetenv("TEST_DEFAULT_LOC")
	})

	t.Run("default locations fallback malformed", func(t *testing.T) {
		t.Setenv("ENV_FILE", "") 
		buf.Reset()
		
		cwd, _ := os.Getwd()
		defaultEnvFile := filepath.Join(cwd, ".env")
		
		os.WriteFile(defaultEnvFile, []byte("MALFORMED_LINE\n"), 0644)
		defer os.Remove(defaultEnvFile)
		
		LoadDotenv()
		
		if !strings.Contains(buf.String(), "Warning: error loading .env file") {
			t.Errorf("expected warning for malformed default file, got: %s", buf.String())
		}
	})
}
