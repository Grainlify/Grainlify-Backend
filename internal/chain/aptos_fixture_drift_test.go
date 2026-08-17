package chain

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The Aptos-Contracts repository holds the same Merkle vectors as Move literals,
// because Move cannot read a file at runtime and the package must build with no
// reference to this tree.
//
// That leaves a copy, and a copy can drift. These two tests catch it, and they
// live on this side deliberately: the constraint is that the *contracts* must not
// depend on the Go tree, not the reverse.
//
// **Read this before relying on them.** Aptos-Contracts is a separate
// repository, checked out as a sibling of this one. So these tests only run on a
// machine that happens to have both side by side, and they SKIP everywhere else -
// including in CI, which checks out one repository. They are a developer-machine
// convenience, not a merge gate.
//
// What actually pins the two repositories to each other is the vectors
// themselves: each side asserts the same digests independently, so a divergence
// turns one of the two suites red wherever it runs. These tests only shorten the
// feedback loop by catching an edit to the copy before the Move suite is run at
// all.
//
// Skipping rather than failing on an absent sibling is therefore correct, and is
// also why neither test is allowed to be the only thing checking a value.
const aptosContractsDir = "../../../Aptos-Contracts"

func aptosContractsPath(t *testing.T, rel string) (string, bool) {
	t.Helper()
	p := filepath.Join(aptosContractsDir, rel)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// TestAptosFixture_IsAByteForByteCopy is the cheap half: a human diffing two
// repositories should be comparing identical files, not reconciling two
// renderings of the same data.
func TestAptosFixture_IsAByteForByteCopy(t *testing.T) {
	copyPath, ok := aptosContractsPath(t, "fixtures/leaf_vector.json")
	if !ok {
		t.Skip("Aptos-Contracts is not checked out beside this repository")
	}

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
	movePath, ok := aptosContractsPath(t, "tests/tree_vectors.move")
	if !ok {
		t.Skip("Aptos-Contracts is not checked out beside this repository")
	}

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

	if len(found) == 0 {
		t.Fatal("no root literals found in the Move fixture; the regex or the file shape changed")
	}
	// Assert how much was checked, not only that what was checked agreed - a
	// restructure that renamed the accessors would otherwise reduce this test's
	// coverage to zero while it kept passing.
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
