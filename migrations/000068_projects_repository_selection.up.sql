-- Whether the maintainer chose specific repositories, or handed over all of them.
--
-- GitHub's /installation/repositories returns `repository_selection` as
-- "all" or "selected" on every response. We decoded only the `repositories`
-- array and dropped it - the fourth field found parsed-and-discarded from that
-- boundary, after fork, owner.type and description.
--
-- It matters because "All repositories" is GitHub's default AND covers future
-- repos, so most installations express no choice at all: every repo the
-- account creates from now on becomes a Grainlify project automatically. But
-- some maintainers DID pick specific repositories, and until now the product
-- could not tell the difference - it indexed everything either way, overriding
-- a choice that had already been made.
--
-- NULL means "not yet observed", not "all". The distinction matters: a row we
-- have never asked about must not be read as consent.
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS installation_repository_selection TEXT
    CHECK (installation_repository_selection IN ('all', 'selected'));

COMMENT ON COLUMN projects.installation_repository_selection IS
  'GitHub repository_selection for the installation this project arrived through: all | selected | NULL (not yet observed).';
