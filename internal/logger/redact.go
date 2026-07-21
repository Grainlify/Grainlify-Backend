package logger

import (
	"strings"
)

var sensitiveKeySubstrings = []string{
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

// RedactString redacts a sensitive string, replacing it with a placeholder.
// It retains the first 4 characters for identification if the string is long enough.
// This is useful for redacting secrets, tokens, or PII.
func RedactString(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 8 {
		return s[:4] + "...[REDACTED]"
	}
	return "[REDACTED]"
}

// RedactMap takes a map of arguments and returns a new map where values
// associated with sensitive keys are redacted. Nested maps are also processed.
func RedactMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		kLower := strings.ToLower(k)
		isSensitive := false
		for _, keyword := range sensitiveKeySubstrings {
			if strings.Contains(kLower, keyword) {
				isSensitive = true
				break
			}
		}
		if isSensitive {
			out[k] = "[REDACTED]"
		} else if nestedMap, ok := v.(map[string]interface{}); ok {
			out[k] = RedactMap(nestedMap)
		} else {
			out[k] = v
		}
	}
	return out
}

