package cli

import "testing"

// The two spellings are not the same, and nothing else in this repo has to
// care: GoReleaser stamps {{ .Version }}, which drops the leading v, while a
// release tag keeps it. Compared raw, an up-to-date sal reports itself behind
// and then downloads a byte-identical copy of itself on every run — which
// looks like the command not working rather than like a comparison bug.
func TestSameReleaseIgnoresTheTagsLeadingV(t *testing.T) {
	for _, c := range []struct {
		current, tag string
		want         bool
	}{
		{"0.2.1", "v0.2.1", true},  // what a released binary actually looks like
		{"v0.2.1", "v0.2.1", true}, // and what a hand-stamped one looks like
		{"0.2.1", "v0.2.2", false},
		{"0.2.1", "v0.10.0", false}, // not a prefix comparison
		{"dev", "v0.2.1", false},
		{"dev+abc1234", "v0.2.1", false},
	} {
		if got := sameRelease(c.current, c.tag); got != c.want {
			t.Errorf("sameRelease(%q, %q) = %v, want %v", c.current, c.tag, got, c.want)
		}
	}
}

// One line must not say 0.2.1 while the next says v0.2.1 about the same thing.
func TestTaggedSpellsAVersionLikeATag(t *testing.T) {
	for in, want := range map[string]string{
		"0.2.1":      "v0.2.1",
		"v0.2.1":     "v0.2.1",
		"dev":        "dev",
		"dev+abc123": "dev+abc123", // never "vdev+abc123"
	} {
		if got := tagged(in); got != want {
			t.Errorf("tagged(%q) = %q, want %q", in, got, want)
		}
	}
}
