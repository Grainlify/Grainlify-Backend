package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func seedMetaProject(t *testing.T, d *db.DB, owner uuid.UUID, name, installation string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status, needs_metadata, github_app_installation_id)
VALUES ($1, $2, 'verified', true, $3) RETURNING id`, owner, name, installation).Scan(&id); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id) })
	return id
}

func metaOwner(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor','meta-backfill',$1) RETURNING id
`, int64(uuid.New().ID())).Scan(&id); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func TestMetadataBackfill_FillsDescriptionFromGitHub(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := metaOwner(t, d)
	id := seedMetaProject(t, d, owner, "acme-"+uuid.NewString()[:8]+"/repo", "inst-"+uuid.NewString()[:8])

	b := &MetadataBackfiller{
		db:    d,
		token: func(context.Context, string) (string, error) { return "t", nil },
		fetch: func(context.Context, string, string) (string, []string, error) {
			return "a real description from GitHub", nil, nil
		},
	}
	b.Run(ctx)

	var description *string
	if err := d.Pool.QueryRow(ctx, `SELECT description FROM projects WHERE id = $1`, id).Scan(&description); err != nil {
		t.Fatalf("read: %v", err)
	}
	if description == nil || *description != "a real description from GitHub" {
		t.Errorf("description = %v; the field GitHub returned was still not stored", description)
	}
}

// The property that keeps this safe to run at boot.
//
// needs_metadata is set by a human opening the setup form, and it is the only
// signal of intent in a pipeline where everything arrives through "All
// repositories" and nobody selects anything. Filling in a description must not
// promote a project nobody chose.
func TestMetadataBackfill_NeverChangesWhatIsLive(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := metaOwner(t, d)
	id := seedMetaProject(t, d, owner, "acme-"+uuid.NewString()[:8]+"/repo", "inst-"+uuid.NewString()[:8])

	b := &MetadataBackfiller{
		db:    d,
		token: func(context.Context, string) (string, error) { return "t", nil },
		fetch: func(context.Context, string, string) (string, []string, error) { return "filled in", nil, nil },
	}
	b.Run(ctx)

	var needsMetadata bool
	var status string
	if err := d.Pool.QueryRow(ctx,
		`SELECT needs_metadata, status FROM projects WHERE id = $1`, id).Scan(&needsMetadata, &status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !needsMetadata {
		t.Error("needs_metadata was cleared - the backfill promoted a project nobody chose. " +
			"That flag is the only intent signal in the pipeline; everything else arrives via " +
			"\"All repositories\" with no selection.")
	}
	if status != "verified" {
		t.Errorf("status changed to %q", status)
	}
}

// An unreadable installation or repo leaves the row alone rather than writing
// an empty description over nothing. Same principle as the fork backfill.
func TestMetadataBackfill_UnreadableProjectsAreLeftAlone(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := metaOwner(t, d)

	for _, tc := range []struct {
		name string
		b    func(*MetadataBackfiller)
	}{
		{"token fails", func(b *MetadataBackfiller) {
			b.token = func(context.Context, string) (string, error) { return "", fmt.Errorf("404") }
			b.fetch = func(context.Context, string, string) (string, []string, error) { return "x", nil, nil }
		}},
		{"repo fetch fails", func(b *MetadataBackfiller) {
			b.token = func(context.Context, string) (string, error) { return "t", nil }
			b.fetch = func(context.Context, string, string) (string, []string, error) {
				return "", nil, fmt.Errorf("403")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedMetaProject(t, d, owner, "acme-"+uuid.NewString()[:8]+"/repo", "inst-"+uuid.NewString()[:8])
			b := &MetadataBackfiller{db: d}
			tc.b(b)
			b.Run(ctx)

			var description *string
			if err := d.Pool.QueryRow(ctx, `SELECT description FROM projects WHERE id = $1`, id).Scan(&description); err != nil {
				t.Fatalf("read: %v", err)
			}
			// NULL, not "". Writing an empty string is still a write: it
			// records "GitHub says this has no description" for a repo nobody
			// managed to ask, and the next run skips it because the row no
			// longer looks pending.
			if description != nil {
				t.Errorf("description = %q for a repo we could not read; the row must be left untouched", *description)
			}
		})
	}
}
