package logger

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRedactMapAllSensitiveKeywords(t *testing.T) {
	t.Parallel()

	keywords := []string{
		"address",
		"amount",
		"email",
		"secret",
		"token",
		"password",
		"private_key",
		"privatekey",
		"signature",
		"sig",
		"authorization",
		"cookie",
		"jwt",
		"api_key",
		"apikey",
		"credential",
	}

	payload := make(map[string]interface{})
	for _, kw := range keywords {
		payload["test_"+kw+"_val"] = "sensitive-" + kw
	}

	redacted := RedactMap(payload)

	for _, kw := range keywords {
		key := "test_" + kw + "_val"
		got, ok := redacted[key]
		if !ok {
			t.Errorf("key %q missing in redacted output", key)
			continue
		}
		if got != "[REDACTED]" {
			t.Errorf("key %q with keyword %q was not redacted: got %v, want %q", key, kw, got, "[REDACTED]")
		}
	}
}

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
		"my-pass-123",
		"priv-key-content",
		"sig_abc_123",
		"Bearer xyz123",
		"session_id=123",
		"eyJhbGciOi...",
		"secret_api_key",
		"user_cred_data",
	}

	payload := map[string]interface{}{
		"ADDRESS": secretValues[0],
		"metadata": map[string]interface{}{
			"nestedAmount": secretValues[1],
			"profile": map[string]interface{}{
				"ContactEmail": secretValues[2],
				"user_info": map[string]interface{}{
					"apiSECRET":        secretValues[3],
					"SessionToken":     secretValues[4],
					"UserPassword":     secretValues[11],
					"my_private_key":   secretValues[12],
					"DigitalSignature": secretValues[13],
					"Authorization":    secretValues[14],
					"SessionCookie":    secretValues[15],
					"UserJWT":          secretValues[16],
					"X_API_KEY":        secretValues[17],
					"UserCredential":   secretValues[18],
				},
				"firstName":   secretValues[5],
				"lastName":    secretValues[6],
				"fullName":    secretValues[7],
				"dob":         secretValues[8],
				"documentNum": secretValues[9],
				"phone":       secretValues[10],
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

	assertPathEquals(t, redacted, []interface{}{"ADDRESS"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "nestedAmount"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "ContactEmail"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "apiSECRET"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "SessionToken"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "UserPassword"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "my_private_key"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "DigitalSignature"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "Authorization"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "SessionCookie"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "UserJWT"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "X_API_KEY"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "user_info", "UserCredential"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "firstName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "lastName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "fullName"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "dob"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "documentNum"}, "[REDACTED]")
	assertPathEquals(t, redacted, []interface{}{"metadata", "profile", "phone"}, "[REDACTED]")
}

func TestRedactErrorRedactsSensitiveValuesInMessage(t *testing.T) {
	t.Parallel()

	err := errors.New(`kyc lookup failed: email="person@example.com" not found`)
	got := RedactError(err)

	if strings.Contains(got, "person@example.com") {
		t.Fatalf("RedactError left the email in the message: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("RedactError did not redact the sensitive value: %q", got)
	}
}

func TestRedactErrorHandlesNil(t *testing.T) {
	t.Parallel()

	if got := RedactError(nil); got != "" {
		t.Fatalf("RedactError(nil) = %q, want empty string", got)
	}
}

func TestRedactMapLeavesNonSensitiveFieldsUnchanged(t *testing.T) {
	t.Parallel()

	payload := map[string]interface{}{
		"project_name": "Grainlify",
		"userID":       "user-123",
		"status":       "active",
		"retryCount":   3,
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

func TestRedactMapRedactsSensitiveKeysInsideSliceValues(t *testing.T) {
	t.Parallel()

	secretValues := []string{
		"alice@example.com",
		"bob@example.com",
		"secret-token-123",
	}

	payload := map[string]interface{}{
		"accounts": []interface{}{
			map[string]interface{}{
				"email":    secretValues[0],
				"username": "alice",
			},
			map[string]interface{}{
				"email":    secretValues[1],
				"username": "bob",
			},
		},
		"sessions": []interface{}{
			map[string]interface{}{
				"accessToken": secretValues[2],
				"expiresIn":   3600,
			},
		},
		"metadata": map[string]interface{}{
			"nestedArray": []interface{}{
				map[string]interface{}{
					"secretKey": "nested-secret",
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

	// Verify nested secret is also redacted
	if strings.Contains(redactedText, "nested-secret") {
		t.Fatalf("redacted payload contains nested secret: %s", redactedText)
	}

	// Verify non-sensitive fields are preserved
	assertPathEquals(t, redacted, []interface{}{"accounts", 0, "username"}, "alice")
	assertPathEquals(t, redacted, []interface{}{"accounts", 1, "username"}, "bob")
	assertPathEquals(t, redacted, []interface{}{"sessions", 0, "expiresIn"}, 3600)
}

func assertPathEquals(t *testing.T, payload map[string]interface{}, path []interface{}, want interface{}) {
	t.Helper()

	var current interface{} = payload
	for _, key := range path {
		if idx, ok := key.(int); ok {
			slice, ok := current.([]interface{})
			if !ok {
				t.Fatalf("path %v expected slice at key %v, got %#v", path, key, current)
			}
			current = slice[idx]
		} else {
			currentMap, ok := current.(map[string]interface{})
			if !ok {
				t.Fatalf("path %v reached non-map value %#v at key %v", path, current, key)
			}
			current = currentMap[key.(string)]
		}
	}

	if current != want {
		t.Fatalf("path %v = %#v, want %#v", path, current, want)
	}
}
