package version

import "testing"

// The line 1.10.0 draws, in both directions. Below it a deployment must vendor
// the base addons or it has no internal-host block at all; at or above it the
// image carries them and a vendored copy is skipped with a warning.
func TestStackBakesAddons(t *testing.T) {
	for tag, want := range map[string]bool{
		"v1.9.0":  false,
		"v1.9.2":  false,
		"v1.10.0": true,
		"v1.10.1": true,
		"v2.0.0":  true,
		// Not a release tag, so it takes the vendoring branch — the direction
		// that fails closed, and the one check-drift.sh takes too.
		"main": false,
		"":     false,
	} {
		if got := StackBakesAddons(tag); got != want {
			t.Errorf("StackBakesAddons(%q) = %v, want %v", tag, got, want)
		}
	}
}
