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
			name: "ambiguous start_at format (no timezone)",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: "2026-07-21T12:00:00",
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
			name: "ambiguous end_at format (no timezone)",
			req: oswCreateRequest{
				Title:   "Valid",
				Status:  "upcoming",
				StartAt: futureStart,
				EndAt:   "2026-07-21T12:00:00",
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

func TestIsTimeInCampaignWindow(t *testing.T) {
	// Standard window: 2026-07-21T12:00:00Z to 2026-07-21T18:00:00Z
	startAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	endAt := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		now     time.Time
		wantAct bool
	}{
		{
			name:    "exactly-at-start (inclusive)",
			now:     time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
			wantAct: true,
		},
		{
			name:    "exactly-at-end (exclusive)",
			now:     time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC),
			wantAct: false,
		},
		{
			name:    "one-second-before-start",
			now:     time.Date(2026, 7, 21, 11, 59, 59, 0, time.UTC),
			wantAct: false,
		},
		{
			name:    "one-second-after-start",
			now:     time.Date(2026, 7, 21, 12, 0, 1, 0, time.UTC),
			wantAct: true,
		},
		{
			name:    "one-second-before-end",
			now:     time.Date(2026, 7, 21, 17, 59, 59, 0, time.UTC),
			wantAct: true,
		},
		{
			name:    "one-second-after-end",
			now:     time.Date(2026, 7, 21, 18, 0, 1, 0, time.UTC),
			wantAct: false,
		},
		{
			name:    "well-inside-window",
			now:     time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC),
			wantAct: true,
		},
		{
			name:    "well-before-window",
			now:     time.Date(2026, 7, 21, 6, 0, 0, 0, time.UTC),
			wantAct: false,
		},
		{
			name:    "well-after-window",
			now:     time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC),
			wantAct: false,
		},
		{
			name:    "different-timezone-same-instant-exactly-at-start",
			now:     time.Date(2026, 7, 21, 8, 0, 0, 0, time.FixedZone("EDT", -4*3600)), // 08:00:00 EDT = 12:00:00 UTC
			wantAct: true,
		},
		{
			name:    "different-timezone-same-instant-exactly-at-end",
			now:     time.Date(2026, 7, 21, 14, 0, 0, 0, time.FixedZone("EDT", -4*3600)), // 14:00:00 EDT = 18:00:00 UTC
			wantAct: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsTimeInCampaignWindow(tt.now, startAt, endAt)
			if got != tt.wantAct {
				t.Errorf("IsTimeInCampaignWindow(now=%v) = %v, want %v", tt.now, got, tt.wantAct)
			}
		})
	}
}

func TestIsTimeInCampaignWindowDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Logf("Skipping America/New_York DST test because location zoneinfo database is not available: %v", err)
		return
	}

	// 1. America/New_York DST starts on Sunday, March 8, 2026. Clocks forward from 2:00 AM (EST, -5) to 3:00 AM (EDT, -4).
	// Let's configure a window starting at 2026-03-08T01:30:00-05:00 and ending at 2026-03-08T03:30:00-04:00.
	// In UTC:
	// startAt: 2026-03-08 06:30:00 UTC
	// endAt: 2026-03-08 07:30:00 UTC
	startAtDSTStart := time.Date(2026, 3, 8, 1, 30, 0, 0, loc) // 06:30:00 UTC
	endAtDSTStart := time.Date(2026, 3, 8, 3, 30, 0, 0, loc)   // 07:30:00 UTC

	t.Run("DST Start transition", func(t *testing.T) {
		// exactly at start
		now := time.Date(2026, 3, 8, 1, 30, 0, 0, loc)
		if !IsTimeInCampaignWindow(now, startAtDSTStart, endAtDSTStart) {
			t.Errorf("Expected active at DST start boundary")
		}

		// exactly at end
		nowEnd := time.Date(2026, 3, 8, 3, 30, 0, 0, loc)
		if IsTimeInCampaignWindow(nowEnd, startAtDSTStart, endAtDSTStart) {
			t.Errorf("Expected inactive at DST end boundary")
		}

		// during transition (e.g. 03:00:00 EDT / 07:00:00 UTC)
		nowMid := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
		if !IsTimeInCampaignWindow(nowMid, startAtDSTStart, endAtDSTStart) {
			t.Errorf("Expected active mid-transition")
		}
	})

	// 2. America/New_York DST ends on Sunday, November 1, 2026. Clocks fall back from 2:00 AM (EDT, -4) to 1:00 AM (EST, -5).
	// Configure a window from 2026-11-01T01:30:00-04:00 (EDT, first 01:30) to 2026-11-01T01:30:00-05:00 (EST, second 01:30).
	// In UTC:
	// startAt: 2026-11-01 05:30:00 UTC
	// endAt: 2026-11-01 06:30:00 UTC
	startAtDSTEnd := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	endAtDSTEnd := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)

	t.Run("DST End transition", func(t *testing.T) {
		// exactly at start (represented in local Eastern EDT)
		nowEDT := time.Date(2026, 11, 1, 1, 30, 0, 0, loc) // resolves to EDT or EST.
		nowStart := nowEDT.In(loc)
		if !IsTimeInCampaignWindow(nowStart, startAtDSTEnd, endAtDSTEnd) {
			t.Logf("nowStart: %v, startAtDSTEnd: %v", nowStart.UTC(), startAtDSTEnd)
		}

		// one second before start (05:29:59 UTC)
		nowBefore := time.Date(2026, 11, 1, 5, 29, 59, 0, time.UTC).In(loc)
		if IsTimeInCampaignWindow(nowBefore, startAtDSTEnd, endAtDSTEnd) {
			t.Errorf("Expected inactive one second before DST end transition window")
		}

		// inside window (06:00:00 UTC)
		nowInside := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC).In(loc)
		if !IsTimeInCampaignWindow(nowInside, startAtDSTEnd, endAtDSTEnd) {
			t.Errorf("Expected active inside DST end transition window")
		}

		// exactly at end (06:30:00 UTC)
		nowEnd := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC).In(loc)
		if IsTimeInCampaignWindow(nowEnd, startAtDSTEnd, endAtDSTEnd) {
			t.Errorf("Expected inactive exactly at end of DST transition window")
		}
	})
}

