package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shaped like a real entry's allowlist at stack 1.13.0 — prose, one live line,
// an OPTIONAL rule, suggestions commented out with their own explanations —
// under an invented provider name. A fixture naming a real bank entry would
// fail internal/invariants, which is the guard on this repo having no
// per-provider knowledge, and it caught exactly that while this was written.
const entryFile = `# Telegraph — egress this entry needs.
#
# METHODS is not optional in practice. Omitting it defaults the entry to
# GET,HEAD,OPTIONS, and every request this provider's client makes is a POST.

api.telegraph.test       GET,POST

# GET is here for listing, which some clients call at startup.

# ---------------------------------------------------------------------------
# OPTIONAL — nothing below is needed for the provider to work.
# ---------------------------------------------------------------------------
# flags.telegraph.test     POST    # feature flags; the client works without it
# errors.telegraph.test    POST    # error reporting
#
# Note what these are NOT: they belong in an allowlist and must never appear in
# the entry's ` + "`hosts`" + `.
`

func TestOnlyUncommentedLinesAreEnabled(t *testing.T) {
	e := Parse([]byte(entryFile))

	if len(e.Enabled) != 1 || e.Enabled[0].Text != "api.telegraph.test       GET,POST" {
		t.Fatalf("enabled = %#v, want the one uncommented entry", e.Enabled)
	}

	// THE rule that makes seeding safe to do by default. A commented line is a
	// suggestion; writing one would grant egress a vendor wanted and the
	// operator never typed.
	for _, l := range e.Enabled {
		if strings.Contains(l.Text, "flags.") || strings.Contains(l.Text, "errors.") {
			t.Errorf("%q was treated as enabled; it is commented out", l.Text)
		}
	}
}

func TestSuggestionsAreReportedWithoutBeingGranted(t *testing.T) {
	e := Parse([]byte(entryFile))

	var hosts []string
	for _, l := range e.Optional {
		hosts = append(hosts, l.Host())
	}
	got := strings.Join(hosts, " ")
	if got != "flags.telegraph.test errors.telegraph.test" {
		t.Errorf("optional hosts = %q, want the two commented entries", got)
	}

	// Prose below the marker is not a destination. Getting this wrong makes a
	// listing noisier, never more permissive — but a listing nobody trusts is
	// one nobody reads.
	for _, l := range e.Optional {
		if strings.Contains(l.Text, "Note what these") {
			t.Errorf("prose %q was reported as a destination", l.Text)
		}
	}
	if len(e.Optional) != 2 {
		t.Errorf("optional = %#v, want exactly the two suggestions", e.Optional)
	}
}

func TestWriteLeavesTheOperatorsLinesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	const mine = "# my own policy\ninternal.example.com    GET\n"
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	e := Parse([]byte(entryFile))
	if _, err := Write(path, "telegraph", e.Enabled); err != nil {
		t.Fatal(err)
	}

	body := read(t, path)
	if !strings.Contains(body, "internal.example.com    GET") {
		t.Error("the operator's own line was lost")
	}
	if !strings.Contains(body, "api.telegraph.test       GET,POST") {
		t.Error("the entry's line was not written")
	}
	if !strings.Contains(body, "# --- sal:telegraph ---") {
		t.Error("no marked block, so a later removal cannot tell what it owns")
	}
}

func TestWriteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	e := Parse([]byte(entryFile))

	if _, err := Write(path, "telegraph", e.Enabled); err != nil {
		t.Fatal(err)
	}
	first := read(t, path)
	if _, err := Write(path, "telegraph", e.Enabled); err != nil {
		t.Fatal(err)
	}
	if second := read(t, path); second != first {
		t.Errorf("a second write changed the file:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// An upgrade re-runs this against a newer release. A host the entry no longer
// needs has to go, or the deployment keeps permitting a destination nothing
// asks for — the same stale-grant problem a left-behind cred-gateway config
// causes, one control over.
func TestWriteReplacesTheBlockRatherThanAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	if _, err := Write(path, "acme", []Line{{Text: "old.example.com   GET"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, "acme", []Line{{Text: "new.example.com   POST"}}); err != nil {
		t.Fatal(err)
	}

	body := read(t, path)
	if strings.Contains(body, "old.example.com") {
		t.Error("the superseded destination is still permitted")
	}
	if !strings.Contains(body, "new.example.com   POST") {
		t.Error("the new destination was not written")
	}
	if strings.Count(body, "# --- sal:acme ---") != 1 {
		t.Errorf("block written twice:\n%s", body)
	}
}

// Removing a provider must close the egress it opened. A line left behind
// keeps permitting a destination whose provider is gone, which is the widened
// boundary nothing would report.
func TestRemoveTakesItsOwnBlockAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(path, []byte("mine.example.com  GET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test   POST"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, "other", []Line{{Text: "api.other.test  GET"}}); err != nil {
		t.Fatal(err)
	}

	removed, err := Remove(path, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Host() != "api.acme.test" {
		t.Errorf("removed = %#v, want acme's one line", removed)
	}

	body := read(t, path)
	for _, want := range []string{"mine.example.com  GET", "api.other.test  GET"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q was removed and should not have been", want)
		}
	}
	if strings.Contains(body, "api.acme.test") {
		t.Error("acme's destination is still permitted after removal")
	}
}

// Someone deleting the end marker must not turn a removal into a no-op that
// silently leaves the grant open. Taking too much is visible; leaving egress
// open is not.
func TestAnUnterminatedBlockIsStillRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	body := "keep.example.com GET\n" + begin("acme") + "\napi.acme.test POST\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(path, "acme"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if strings.Contains(got, "api.acme.test") {
		t.Errorf("an unterminated block left the destination permitted:\n%s", got)
	}
	if !strings.Contains(got, "keep.example.com GET") {
		t.Error("the operator's line was taken with it")
	}
}

func TestBlocksSaysWhichEntryOwnsWhat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(path, []byte("hand.example.com GET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test POST"}}); err != nil {
		t.Fatal(err)
	}

	owned, err := Blocks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned["acme"]) != 1 || owned["acme"][0].Host() != "api.acme.test" {
		t.Errorf("owned = %#v", owned)
	}
	// A hand-written line belongs to nobody, which is what lets a listing say
	// so rather than attributing it to whichever entry happens to be nearest.
	for name, lines := range owned {
		for _, l := range lines {
			if l.Host() == "hand.example.com" {
				t.Errorf("the operator's own line was attributed to %q", name)
			}
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
