package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotenv(t *testing.T) {
	// Create a temporary directory for our .env files
	tempDir := t.TempDir()

	tests := []struct {
		name           string
		envContent     string
		expectedEnv    map[string]string
		notExpectedEnv []string
		expectLogMsg   string
	}{
		{
			name: "Valid file",
			envContent: `A=1
B="multi
line"
C=3`,
			expectedEnv: map[string]string{
				"A": "1",
				"B": "multi\nline",
				"C": "3",
			},
		},
		{
			name: "Missing equals sign",
			envContent: `A=1
BAD_LINE
B=2`,
			expectedEnv: map[string]string{
				"A": "1",
				"B": "2",
			},
			expectLogMsg: `WARNING: malformed line in`,
		},
		{
			name: "Unterminated quote",
			envContent: `A=1
B="unterminated
C=3`,
			expectedEnv: map[string]string{
				"A": "1",
			},
			notExpectedEnv: []string{"B", "C"},
			expectLogMsg:   `WARNING: malformed or unterminated line in`,
		},
		{
			name: "Duplicate key (last-wins)",
			envContent: `A=1
A=2`,
			expectedEnv: map[string]string{
				"A": "2",
			},
		},
		{
			name:       "Empty file",
			envContent: ``,
			expectedEnv: map[string]string{},
		},
		{
			name: "Comment-only line",
			envContent: `# This is a comment
# Another comment`,
			expectedEnv: map[string]string{},
		},
		{
			name: "Mixed with comments",
			envContent: `A=1
# A comment
B=2`,
			expectedEnv: map[string]string{
				"A": "1",
				"B": "2",
			},
		},
		{
			name: "Does not overwrite existing OS env",
			envContent: `A=1`,
			// "A" is set to "EXISTING" in test setup
			expectedEnv: map[string]string{
				"A": "EXISTING",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clear all environment variables for a clean state
			os.Clearenv()
			
			if tc.name == "Does not overwrite existing OS env" {
				os.Setenv("A", "EXISTING")
			}

			// Write the env content to a file
			envPath := filepath.Join(tempDir, ".env")
			if err := os.WriteFile(envPath, []byte(tc.envContent), 0644); err != nil {
				t.Fatalf("failed to write .env file: %v", err)
			}

			// Capture log output
			var logBuf bytes.Buffer
			log.SetOutput(&logBuf)
			defer log.SetOutput(os.Stderr)

			// Tell LoadDotenv to use our specific file
			os.Setenv("ENV_FILE", envPath)

			LoadDotenv()

			logOutput := logBuf.String()
			
			// Verify expected env variables
			for k, v := range tc.expectedEnv {
				actual := os.Getenv(k)
				if actual != v {
					t.Errorf("expected env %s=%q, got %q", k, v, actual)
				}
			}

			// Verify not expected env variables
			for _, k := range tc.notExpectedEnv {
				if val, exists := os.LookupEnv(k); exists {
					t.Errorf("expected env %s to not be set, but got %q", k, val)
				}
			}

			// Verify log messages
			if tc.expectLogMsg != "" && !strings.Contains(logOutput, tc.expectLogMsg) {
				t.Errorf("expected log output to contain %q, got: %q", tc.expectLogMsg, logOutput)
			} else if tc.expectLogMsg == "" && logOutput != "" {
				t.Errorf("expected no log output, got: %q", logOutput)
			}
		})
	}
}
