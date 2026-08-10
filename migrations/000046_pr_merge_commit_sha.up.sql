-- The commit a PR was merged as. Needed so §5.5's citation requirement is
-- actually checkable: a verdict cites "src/auth/login.go:44-61", and the
-- reviewer's link has to point at that file *as merged*, not at a branch
-- that has moved since. Without a fixed ref the citation drifts and the
-- requirement quietly becomes decorative.
ALTER TABLE github_pull_requests ADD COLUMN IF NOT EXISTS merge_commit_sha TEXT;
