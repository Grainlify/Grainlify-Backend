package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateCreate(t *testing.T) {
	// Reference a future date for valid start/end
	futureStart := time.Now().Add(24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	futureEnd := time.Now().Add(48 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

	tests := []struct {
		name string
		req  oswCreateRequest
		want string // expected error code; empty string means pass
	}{
		{
			name: "valid minimal",
			req: oswCreateRequest{
				Title:   "Valid Title",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "",
		},
		{
			name: "valid with all fields",
			req: oswCreateRequest{
				Title:       "Open Source Week 2026",
				Description: "A great event",
				Location:    "Online",
				Status:      "running",
				StartAt:     futureStart,
				EndAt:       futureEnd,
			},
			want: "",
		},
		{
			name: "valid draft",
			req: oswCreateRequest{
				Title:   "Draft Event",
				Status:  "draft",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "",
		},
		{
			name: "valid completed",
			req: oswCreateRequest{
				Title:   "Past Event",
				Status:  "completed",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "",
		},
		{
			name: "missing title",
			req: oswCreateRequest{
				Title:   "",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "title_required",
		},
		{
			name: "title only whitespace",
			req: oswCreateRequest{
				Title:   "   ",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "title_required",
		},
		{
			name: "title too long",
			req: oswCreateRequest{
				Title:   strings.Repeat("A", maxTitleLen+1),
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "title_too_long",
		},
		{
			name: "title exactly at max length",
			req: oswCreateRequest{
				Title:   strings.Repeat("A", maxTitleLen),
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "",
		},
		{
			name: "description too long",
			req: oswCreateRequest{
				Title:       "Valid",
				Description: strings.Repeat("A", maxDescriptionLen+1),
				Status:      "upcoming",
				StartAt:     futureStart,
				EndAt:       futureEnd,
			},
			want: "description_too_long",
		},
		{
			name: "description exactly at max length",
			req: oswCreateRequest{
				Title:       "Valid",
				Description: strings.Repeat("A", maxDescriptionLen),
				Status:      "upcoming",
				StartAt:     futureStart,
				EndAt:       futureEnd,
			},
			want: "",
		},
		{
			name: "location too long",
			req: oswCreateRequest{
				Title:    "Valid",
				Location: strings.Repeat("A", maxLocationLen+1),
				Status:   "upcoming",
				StartAt:  futureStart,
				EndAt:    futureEnd,
			},
			want: "location_too_long",
		},
		{
			name: "invalid status",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "invalid_status_value",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "invalid_status",
		},
		{
			name: "invalid start_at format",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: "not-a-date",
				EndAt:   futureEnd,
			},
			want: "invalid_start_at",
		},
		{
			name: "invalid end_at format",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   "also-not-a-date",
			},
			want: "invalid_end_at",
		},
		{
			name: "end_at equals start_at (not after)",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   futureStart,
			},
			want: "end_at_must_be_after_start_at",
		},
		{
			name: "end_at before start_at (reversed range)",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: futureEnd,
				EndAt:   futureStart,
			},
			want: "end_at_must_be_after_start_at",
		},
		{
			name: "status empty defaults - handled by caller not validateCreate",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "",
				StartAt: futureStart,
				EndAt:   futureEnd,
			},
			want: "invalid_status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateCreate(tt.req)
			if got != tt.want {
				t.Errorf("validateCreate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCreateEdgeCases(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	// Whitespace-only description/location should be valid (treated as empty/NULL)
	t.Run("whitespace description and location are valid", func(t *testing.T) {
		req := oswCreateRequest{
			Title:       "Valid",
			Description: "   ",
			Location:    "   ",
			Status:      "draft",
			StartAt:     now.Add(24 * time.Hour).Format(time.RFC3339),
			EndAt:       now.Add(48 * time.Hour).Format(time.RFC3339),
		}
		if got := validateCreate(req); got != "" {
			t.Errorf("expected pass, got %q", got)
		}
	})

	// All valid status values
	for _, s := range []string{"upcoming", "running", "completed", "draft"} {
		t.Run(fmt.Sprintf("status_%s", s), func(t *testing.T) {
			req := oswCreateRequest{
				Title:   fmt.Sprintf("Event %s", s),
				Status:  s,
				StartAt: now.Add(24 * time.Hour).Format(time.RFC3339),
				EndAt:   now.Add(48 * time.Hour).Format(time.RFC3339),
			}
			if got := validateCreate(req); got != "" {
				t.Errorf("status %q should be valid, got %q", s, got)
			}
		})
	}
}
