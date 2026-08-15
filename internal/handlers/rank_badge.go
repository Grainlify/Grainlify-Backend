package handlers

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/ranking"
)

// buildRankBadge assembles the "rank" object both profile endpoints return.
//
// It reports the seasonal standing at the top level (that is what the public
// board shows by default, so it is what the badge must agree with) and nests
// the all-time standing alongside it.
//
// Both, deliberately. The season board is a rolling 90-day window, so a
// contributor whose merges are all older than that is genuinely unranked
// *this season* - but rendering a bare "Unranked" tells them they do not
// count, when what is actually true is that the season reset. Carrying
// all-time in the same payload lets the UI say "Unranked this season ·
// Silver all-time", which is the same data and a completely different
// message. It costs one extra query against an already-cheap table.
//
// This lives in one function rather than in each handler because the two
// profile endpoints previously computed rank with two *different* queries
// that disagreed with each other and with the public board. Extracting the
// SQL into internal/ranking fixed the disagreement; keeping the presentation
// here stops it growing back.
func buildRankBadge(ctx context.Context, pool db.DBPool, githubLogin string) fiber.Map {
	now := time.Now()

	seasonPos, seasonMerged := rankPositionOrNil(ctx, pool, githubLogin, ranking.Options{}, now)
	allTimePos, allTimeMerged := rankPositionOrNil(
		ctx, pool, githubLogin, ranking.Options{Window: ranking.WindowAllTime}, now,
	)

	seasonTier := tierFor(seasonPos)
	allTimeTier := tierFor(allTimePos)

	return fiber.Map{
		// Seasonal, at the top level: these keys predate the window split and
		// existing clients read them, so they keep meaning "the standing that
		// matches the default public board".
		"position":   seasonPos,
		"tier":       string(seasonTier),
		"tier_name":  GetRankTierDisplayName(seasonTier),
		"tier_color": GetRankTierColor(seasonTier),
		// The quantity the position is derived from. contributions_count
		// elsewhere in these responses is total authored activity and is a much
		// larger number; surfacing both stops the badge from looking like it
		// disagrees with the profile around it.
		"merged_prs": seasonMerged,
		"window":     string(ranking.WindowSeason),

		"all_time": fiber.Map{
			"position":   allTimePos,
			"tier":       string(allTimeTier),
			"tier_name":  GetRankTierDisplayName(allTimeTier),
			"tier_color": GetRankTierColor(allTimeTier),
			"merged_prs": allTimeMerged,
		},
	}
}

// emptyRankBadge used to live here: "the badge for someone who is not ranked
// in either window - used where a handler returns a placeholder profile
// without running the ranking at all."
//
// That description is the bug, written down. The one handler that used it -
// PublicProfile's early return for a login with no github_accounts row -
// produced an Unranked badge for contributors the leaderboard was actively
// ranking, because it never ran the ranking to find out. Removed with its
// last caller: buildRankBadge already returns a correctly-shaped badge with a
// nil position for someone genuinely unranked, and it gets that answer by
// asking rather than by assuming.
//
// Do not reintroduce a way to produce a rank without computing one.

// rankPositionOrNil is Position with the error folded into "not ranked".
//
// A rank badge is decoration on a page whose real content is the profile; a
// failed ranking query should degrade to an absent badge, not fail the whole
// request. The error is logged rather than swallowed silently.
func rankPositionOrNil(ctx context.Context, pool db.DBPool, login string, opts ranking.Options, now time.Time) (*int, int) {
	pos, merged, err := ranking.Position(ctx, pool, login, opts, now)
	if err != nil {
		slog.Warn("failed to compute rank position",
			"error", err,
			"github_login", login,
			"window", string(opts.Window),
		)
		return nil, 0
	}
	return pos, merged
}

// tierFor maps a position to its tier, treating "absent from the ranking" as
// Unranked rather than Bronze.
//
// Bronze is a real position (500+) that a contributor earned. Handing it to
// someone with no qualifying merges both overstates them and makes the
// genuine Bronze badge meaningless. The two profile endpoints used to
// disagree about exactly this - one said bronze, the other unranked, for the
// same user.
func tierFor(position *int) RankTier {
	if position == nil || *position <= 0 {
		return RankTierUnranked
	}
	return GetRankTier(*position)
}
