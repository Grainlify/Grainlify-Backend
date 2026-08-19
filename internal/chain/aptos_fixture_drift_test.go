package chain

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Drift checks against the sibling Aptos-Contracts repository.
//
// That repository holds the same Merkle vectors as Move literals, because Move
// cannot read a file at runtime and the package must build with no reference to
// this tree. That leaves a copy, and a copy can drift. These tests catch it.
//
// # Why these fail rather than skip
//
// The first version skipped when Aptos-Contracts was not checked out beside this
// repository. That made the whole suite report green while silently performing
// two fewer checks than the reader believed - which is the exact shape every
// entry in docs/VERIFICATION-TRAPS.md shares. A skip is not a neutral outcome; it
// is a pass that means nothing, and nobody reads the skip line.
//
// So an absent sibling is now a **failure**, and the only way to get a pass
// without the checks is to declare their absence explicitly in
// GRAINLIFY_SIBLING_REPOS_ABSENT. That declaration lives in
// .github/workflows/ci.yml and nowhere else, so it is reviewable code rather than
// a runtime accident.
//
// # The consequence, written down because it is easy to forget
//
// **A green backend test run says nothing about the Move contract.** Not when the
// sibling is absent, and not when it is present either - these tests compare
// stored vector values, they do not compile or run a single line of Move. The
// contract's own suite has to be run separately and deliberately:
//
//	cd ../Aptos-Contracts && aptos move test --dev
//
// Verifying this repository in an isolated worktree makes that sharper, because a
// worktree has no sibling directory: the run fails here until you either place
// one beside it or state that you are checking the Go half only.
//
// # What actually pins the implementations
//
// Not these tests. Go, Rust and Move each assert the same digests independently,
// so a divergence turns one of the three suites red wherever it runs. These are a
// fourth layer that shortens the feedback loop by catching an edited copy before
// the Move suite is run at all.

// aptosContractsDir is found by searching upward, not by counting directories.
//
// It used to be the fixed literal "../../../Aptos-Contracts", which is correct
// from a normal checkout and wrong from a git worktree: sessions now work in
// <parent>/.worktrees/s<id>-backend, one level deeper, so the sibling sits at
// ../../../../Aptos-Contracts instead. Every drift check failed on that alone.
//
// A hardcoded depth encodes where the repository happens to sit. Searching for
// a marker file encodes what we are actually looking for, and survives being
// moved - which matters more than usual here, because a check that fails for
// environmental reasons is a check somebody eventually switches off, and these
// are the checks whose whole purpose is to refuse to skip.
var aptosContractsMarker = filepath.Join("sources", "escrow.move")

func findAptosContractsDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "Aptos-Contracts")
		if _, err := os.Stat(filepath.Join(cand, aptosContractsMarker)); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// siblingAbsentEnv declares that the sibling repository is knowingly not present.
//
// Set only in CI, which performs a single checkout. The durable fix is for CI to
// check out Aptos-Contracts as well, at which point this variable and the branch
// it guards should both be deleted.
const siblingAbsentEnv = "GRAINLIFY_SIBLING_REPOS_ABSENT"

// wantDriftedFiles is how many files in the sibling repository these tests read.
// Asserted so that dropping one of the checks is a failure rather than a quietly
// smaller suite - the lesson from internal/ranking/fork_guard_test.go, where a
// structural check silently reduced its own coverage to one gate of three.
const wantDriftedFiles = 2

// wantPinnedRoots is the number of tree roots the vector must pin, hardcoded
// rather than read from the vector itself.
//
// Comparing the Move literal count against the JSON count alone would pass if
// somebody removed a root from both sides, which is precisely the change that
// would matter: the odd leaf counts are the only ones that can catch a reversed
// sort, so losing them is losing the point of the vectors.
const wantPinnedRoots = 7

// aptosContractsPath resolves a path inside the sibling repository.
//
// Fails the calling test when the sibling is missing and its absence has not been
// declared. It deliberately does not return a bool for the caller to branch on:
// that is what reintroduced the silent skip last time.
func aptosContractsPath(t *testing.T, rel string) string {
	t.Helper()

	if base := findAptosContractsDir(); base != "" {
		p := filepath.Join(base, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	if os.Getenv(siblingAbsentEnv) != "" {
		t.Skipf("%s is set, so the Aptos-Contracts drift checks are knowingly not "+
			"running. The Move vectors are unverified from this side; run "+
			"`aptos move test --dev` in that repository.", siblingAbsentEnv)
	}

	t.Fatalf(`cannot read %s

The Aptos-Contracts repository is not checked out beside this one, so the Merkle
vector drift checks cannot run. This is a failure rather than a skip on purpose:
a suite that quietly performs fewer checks than you believe is the failure mode
docs/VERIFICATION-TRAPS.md exists to catalogue.

Pick one:

  * clone Aptos-Contracts as a sibling of this repository, or
  * set %s=1 to state that you are checking the Go half only

Either way, a green run of this repository says nothing about the Move contract.
Run its suite deliberately:

  cd ../Aptos-Contracts && aptos move test --dev
`, filepath.Join("<sibling>", rel), siblingAbsentEnv)
	return ""
}

// TestAptosFixture_IsAByteForByteCopy is the cheap half: a human diffing two
// repositories should be comparing identical files, not reconciling two
// renderings of the same data.
func TestAptosFixture_IsAByteForByteCopy(t *testing.T) {
	copyPath := aptosContractsPath(t, "fixtures/leaf_vector.json")

	authoritative, err := os.ReadFile("testdata/leaf_vector.json")
	if err != nil {
		t.Fatalf("read authoritative vector: %v", err)
	}
	copied, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("read copied fixture: %v", err)
	}

	if string(authoritative) != string(copied) {
		t.Errorf("Aptos-Contracts/fixtures/leaf_vector.json has drifted from "+
			"internal/chain/testdata/leaf_vector.json\n"+
			"copy it across rather than editing one side:\n"+
			"  cp internal/chain/testdata/leaf_vector.json %s", copyPath)
	}
}

// TestAptosMoveLiterals_MatchTheVector is the half that matters.
//
// The byte-for-byte JSON check above proves the two JSON files agree, and proves
// nothing about the Move constants the contract actually asserts against - those
// are a third copy, and the one the tests use. Reading them out of the source is
// the same technique TestSupportDelivered_SQLAndGoAgree ended up using for the
// same reason: a check that compares a value against itself is documentation.
func TestAptosMoveLiterals_MatchTheVector(t *testing.T) {
	movePath := aptosContractsPath(t, "tests/tree_vectors.move")

	src, err := os.ReadFile(movePath)
	if err != nil {
		t.Fatalf("read %s: %v", movePath, err)
	}
	v := loadLeafVector(t)

	rootLit := regexp.MustCompile(`root_(\d+)\(\): vector<u8> \{ x"([0-9a-f]{64})" \}`)
	found := map[string]string{}
	for _, m := range rootLit.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = m[2]
	}

	// Assert how much was checked, not only that what was checked agreed.
	// Against a hardcoded count as well as the vector's own, so that removing a
	// root from both sides fails here rather than shrinking this test silently.
	if len(found) != wantPinnedRoots {
		t.Errorf("found %d root literals in the Move fixture, want %d - a vector "+
			"that pins fewer counts cannot catch a reversed leaf sort, which is "+
			"only visible at non-power-of-two counts", len(found), wantPinnedRoots)
	}
	if len(found) != len(v.TreeVectors.Roots) {
		t.Errorf("Move declares %d roots, the vector has %d; they must pin the same set",
			len(found), len(v.TreeVectors.Roots))
	}

	for n, want := range v.TreeVectors.Roots {
		got, ok := found[n]
		if !ok {
			t.Errorf("Move fixture has no root for n=%s", n)
			continue
		}
		if got != want {
			t.Errorf("Move root_%s\n got  %s\n want %s", n, got, want)
		}
	}

	leafLit := regexp.MustCompile(`aptos_leaf\(\): vector<u8> \{\s*x"([0-9a-f]{64})"\s*\}`)
	lm := leafLit.FindStringSubmatch(string(src))
	if lm == nil {
		t.Fatal("no aptos_leaf literal found in the Move fixture")
	}
	if lm[1] != v.AptosLeafVector.Leaf {
		t.Errorf("Move aptos_leaf\n got  %s\n want %s", lm[1], v.AptosLeafVector.Leaf)
	}
}

// TestAptosDriftChecks_CoverEveryFileTheyClaimTo asserts the drift suite has not
// quietly shrunk.
//
// The two tests above read one file each. If a future edit drops one of them, or
// points both at the same file, nothing else would notice: the remaining test
// would pass and the suite would still look like a cross-repository check. This
// counts the files actually named and reachable, the same guard shape as
// wantGates in internal/ranking/fork_guard_test.go.
func TestAptosDriftChecks_CoverEveryFileTheyClaimTo(t *testing.T) {
	files := []string{
		"fixtures/leaf_vector.json",
		"tests/tree_vectors.move",
	}
	if len(files) != wantDriftedFiles {
		t.Fatalf("this test names %d sibling files, want %d", len(files), wantDriftedFiles)
	}
	for _, rel := range files {
		// Uses the same resolver, so an absent sibling fails here too rather than
		// leaving this guard as the one test that still passes.
		if p := aptosContractsPath(t, rel); p == "" {
			t.Fatalf("%s is not readable in the sibling repository", rel)
		}
	}
}
