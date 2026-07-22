package logger

import (
	"encoding/json"
	"errors"
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
		"Alice",
		"Smith",
		"Alice Smith",
		"1990-01-01",
		"AB123456C",
		"+1234567890",
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
				"firstName":    secretValues[5],
				"lastName":     secretValues[6],
				"fullName":     secretValues[7],
				"dob":          secretValues[8],
				"documentNum":  secretValues[9],
				"phone":        secretValues[10],
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
	assertPathEquals(t, redacted, []string{"metadata", "profile", "firstName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "lastName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "fullName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "dob"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "documentNum"}, "[REDACTED]")
	assertPathEquals(t, redacted, []string{"metadata", "profile", "phone"}, "[REDACTED]")
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

func TestRedactMapSlices(t *testing.T) {
	t.Parallel()

	payload := map[string]interface{}{
		"usersList": []interface{}{
			map[string]interface{}{
				"first_name": "Bob",
				"last_name":  "Jones",
				"role":       "admin",
			},
			map[string]interface{}{
				"first_name": "Charlie",
				"phone":      "+1987654321",
			},
		},
	}

	redacted := RedactMap(payload)
	
	usersList, ok := redacted["usersList"].([]interface{})
	if !ok || len(usersList) != 2 {
		t.Fatalf("expected usersList slice of size 2, got %#v", redacted["usersList"])
	}

	u1 := usersList[0].(map[string]interface{})
	if u1["first_name"] != "[REDACTED]" || u1["last_name"] != "[REDACTED]" {
		t.Errorf("u1 not redacted: %#v", u1)
	}
	if u1["role"] != "admin" {
		t.Errorf("u1 non-sensitive field changed: %#v", u1)
	}

	u2 := usersList[1].(map[string]interface{})
	if u2["first_name"] != "[REDACTED]" || u2["phone"] != "[REDACTED]" {
		t.Errorf("u2 not redacted: %#v", u2)
	}
}

func TestRedactError(t *testing.T) {
	t.Parallel()

	t.Run("JSON_Error", func(t *testing.T) {
		rawErr := errors.New("didit get decision failed: status 400, body: {\"status\": \"rejected\", \"decision\": {\"first_name\": \"John\", \"last_name\": \"Doe\", \"document_number\": \"12345\"}}")
		redactedStr := RedactError(rawErr)

		if strings.Contains(redactedStr, "John") {
			t.Errorf("expected first_name to be redacted in JSON error, got: %s", redactedStr)
		}
		if strings.Contains(redactedStr, "Doe") {
			t.Errorf("expected last_name to be redacted in JSON error, got: %s", redactedStr)
		}
		if strings.Contains(redactedStr, "12345") {
			t.Errorf("expected document_number to be redacted in JSON error, got: %s", redactedStr)
		}
		if !strings.Contains(redactedStr, "status 400") {
			t.Errorf("expected status code metadata to be preserved, got: %s", redactedStr)
		}
	})

	t.Run("PlainText_Error", func(t *testing.T) {
		rawErr := errors.New("failed because first_name=John and document_number: 12345")
		redactedStr := RedactError(rawErr)

		if strings.Contains(redactedStr, "John") {
			t.Errorf("expected first_name to be redacted in plain error, got: %s", redactedStr)
		}
		if strings.Contains(redactedStr, "12345") {
			t.Errorf("expected document_number to be redacted in plain error, got: %s", redactedStr)
		}
		if !strings.Contains(redactedStr, "failed because") {
			t.Errorf("expected error structure to be preserved, got: %s", redactedStr)
		}
	})
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
