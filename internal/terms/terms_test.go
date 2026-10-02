package terms

import (
	"sort"
	"testing"
)

// Versions are compared as strings ("is what they accepted older than
// current?"), which is only right while every version is a YYYY-MM-DD date
// and the list is in order. A version named "v2" or appended out of order
// would make somebody who accepted the newest text look like they had not.
func TestVersions_AreOrderedDates(t *testing.T) {
	if len(Versions) == 0 {
		t.Fatal("no versions")
	}
	for _, v := range Versions {
		if len(v) != 10 || v[4] != '-' || v[7] != '-' {
			t.Errorf("version %q is not YYYY-MM-DD", v)
		}
	}
	if !sort.StringsAreSorted(Versions) {
		t.Errorf("Versions is not in order: %v", Versions)
	}
	if Current() != Versions[len(Versions)-1] {
		t.Errorf("Current() = %q, want the last listed version", Current())
	}
}

func TestKnown(t *testing.T) {
	if !Known(Current()) {
		t.Error("the current version is not known")
	}
	for _, v := range []string{"", "2099-01-01", "latest", Current() + " "} {
		if Known(v) {
			t.Errorf("Known(%q) = true; only published versions may be recorded", v)
		}
	}
}
