package handlers

import (
	"strings"
	"testing"
)

func TestValidateAvatarURL(t *testing.T) {
	// A small valid 1x1 PNG data URI, well under maxAvatarDataURILen.
	const validPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

	tests := []struct {
		name      string
		avatarURL string
		wantErr   bool
	}{
		{name: "https URL accepted", avatarURL: "https://example.com/avatar.png"},
		{name: "http URL accepted", avatarURL: "http://example.com/avatar.png"},
		{name: "valid data:image/png accepted", avatarURL: validPNG},
		{name: "data:image/jpeg accepted", avatarURL: "data:image/jpeg;base64,/9j/4AAQSkZJRg=="},
		{name: "data:image/gif accepted", avatarURL: "data:image/gif;base64,R0lGODlhAQABAAAAACw="},
		{name: "data:image/webp accepted", avatarURL: "data:image/webp;base64,UklGRhoAAABXRUJQVlA4="},
		{name: "data:image/svg+xml rejected", avatarURL: "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", wantErr: true},
		{name: "data:text/plain rejected", avatarURL: "data:text/plain;base64,aGVsbG8=", wantErr: true},
		{name: "data:image/svg+xml without base64 flag rejected", avatarURL: "data:image/svg+xml,<svg onload=alert(1)>", wantErr: true},
		{name: "ftp URL rejected", avatarURL: "ftp://example.com/avatar.png", wantErr: true},
		{name: "javascript: URI rejected", avatarURL: "javascript:alert(1)", wantErr: true},
		{name: "malformed data URI with no comma rejected", avatarURL: "data:image/png;base64", wantErr: true},
		{name: "data URI with unparseable media type rejected", avatarURL: "data:image/png;;;,abc", wantErr: true},
		{name: "oversized data:image/png rejected", avatarURL: "data:image/png;base64," + strings.Repeat("A", maxAvatarDataURILen), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errCode := validateAvatarURL(tc.avatarURL)
			if tc.wantErr && errCode == "" {
				t.Fatalf("validateAvatarURL(%q) = \"\", want a non-empty error code", tc.avatarURL)
			}
			if !tc.wantErr && errCode != "" {
				t.Fatalf("validateAvatarURL(%q) = %q, want \"\"", tc.avatarURL, errCode)
			}
		})
	}
}

func TestValidateAvatarURL_OversizedPayloadGetsSpecificErrorCode(t *testing.T) {
	oversized := "data:image/png;base64," + strings.Repeat("A", maxAvatarDataURILen)
	if got := validateAvatarURL(oversized); got != "avatar_url_too_large" {
		t.Fatalf("validateAvatarURL(oversized) = %q, want \"avatar_url_too_large\"", got)
	}
}

func TestValidateAvatarURL_SVGGetsInvalidFormatErrorCode(t *testing.T) {
	svg := "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="
	if got := validateAvatarURL(svg); got != "invalid_avatar_url_format" {
		t.Fatalf("validateAvatarURL(svg) = %q, want \"invalid_avatar_url_format\"", got)
	}
}

// TestValidateAvatarURL_ReasonablySizedPNGAcceptedExactlyAsBefore documents
// that a normal-sized data:image/png avatar continues to be accepted and
// returned unchanged, matching pre-fix behavior for the common case.
func TestValidateAvatarURL_ReasonablySizedPNGAcceptedExactlyAsBefore(t *testing.T) {
	png := "data:image/png;base64," + strings.Repeat("A", 1024) // 1KB, well under the cap
	if got := validateAvatarURL(png); got != "" {
		t.Fatalf("validateAvatarURL(1KB png) = %q, want \"\"", got)
	}
}
