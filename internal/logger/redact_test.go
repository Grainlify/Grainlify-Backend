package logger

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRedactMapRedactsSensitiveKeysRegardlessOfCaseAndDepth(t *testing.T) {
	t.Parallel()

	secretValues := []string{
		"0x123456789abcdef",
		"2500000",
		"person@example.com",
		"super-secret-value",
		"ghp_token_value",
	}

	payload := map[string]interface{}{
		"ADDRESS": secretValues[0],
		"metadata": map[string]interface{}{
			"nestedAmount": secretValues[1],
			"profile": map[string]interface{}{
				"ContactEmail": secretValues[2],
				"credentials": map[string]interface{}{
					"apiSECRET":    secretValues[3],
					"SessionToken": secretValues[4],
				},
			},
		},
	}

	redacted := RedactMap(payload)
	redactedJSON, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal redacted payload: %v", err)
	}
	redactedText := string(redactedJSON)

	for _, secretValue := range secretValues {
		if strings.Contains(redactedText, secretValue) {
			t.Fatalf("redacted payload contains original secret value %q: %s", secretValue, redactedText)
		}
	}

	assertPathEquals(t, redacted, []string{"ADDRESS"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "nestedAmount"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "ContactEmail"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "credentials", "apiSECRET"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "credentials", "SessionToken"}, "[REDACTED]")
}

func TestRedactMapLeavesNonSensitiveFieldsUnchanged(t *testing.T) {
	t.Parallel()

	payload := map[string]interface{}{
		"userID":     "user-123",
		"status":     "active",
		"retryCount": 3,
		"metadata": map[string]interface{}{
			"requestID": "req-456",
			"enabled":   true,
		},
	}

	redacted := RedactMap(payload)

	if !reflect.DeepEqual(redacted, payload) {
		t.Fatalf("non-sensitive fields changed: got %#v, want %#v", redacted, payload)
	}
}

func assertPathEquals(t *testing.T, payload map[string]interface{}, path []string, want interface{}) {
	t.Helper()

	var current interface{} = payload
	for _, key := range path {
		currentMap, ok := current.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v reached non-map value %#v", path, current)
		}
		current = currentMap[key]
	}

	if current != want {
		t.Fatalf("path %v = %#v, want %#v", path, current, want)
	}
}
