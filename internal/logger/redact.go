package logger

import (
	"regexp"
	"strings"
)

// redactRegex matches sensitive keys in plain text / log strings and captures the value.
// It matches any word key containing sensitive keywords, followed by non-alphanumeric separators,
// and then the value itself (quoted or unquoted string).
var redactRegex = regexp.MustCompile(`(?i)([a-zA-Z0-9_]*(?:` + strings.Join(sensitiveKeySubstrings, "|") + `)[a-zA-Z0-9_]*)([^a-zA-Z0-9]*?)("[^"]*"|[a-zA-Z0-9_.-]+)`)

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
	"firstname",
	"lastname",
	"fullname",
	"document",
	"dob",
	"birth",
	"phone",
	"mobile",
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

// RedactError returns a redacted string representation of an error, with any
// sensitive key=value patterns in its message (e.g. "email: foo@bar.com")
// replaced by a placeholder. Safe to call with a nil error.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	return redactRegex.ReplaceAllString(err.Error(), "$1$2[REDACTED]")
}

// RedactMap takes a map of arguments and returns a new map where values
// associated with sensitive keys (like "address", "amount", "secret", "token", "email",
// "name", "document", "dob", "birth", "phone", "mobile", or common credential/secret
// keywords) are redacted. Nested maps and slices are also processed.
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
		} else if slice, ok := v.([]interface{}); ok {
			out[k] = redactSlice(slice)
		} else {
			out[k] = v
		}
	}
	return out
}

// redactSlice processes a slice, recursively redacting any map[string]interface{} elements.
func redactSlice(slice []interface{}) []interface{} {
	if slice == nil {
		return nil
	}
	out := make([]interface{}, len(slice))
	for i, v := range slice {
		if nestedMap, ok := v.(map[string]interface{}); ok {
			out[i] = RedactMap(nestedMap)
		} else if nestedSlice, ok := v.([]interface{}); ok {
			out[i] = redactSlice(nestedSlice)
		} else {
			out[i] = v
		}
	}
	return out
}
