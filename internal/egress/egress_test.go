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

// `allowlist allow` puts a line OUTSIDE every block, which is what makes it
// the operator's: it survives `providers remove` and no upgrade rewrites it.
// Added into a block, it would vanish the next time that entry was written,
// with nothing to say why.
func TestAllowWritesOutsideEveryBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test POST"}}); err != nil {
		t.Fatal(err)
	}

	added, err := Allow(path, "operator.test", "*")
	if err != nil || !added {
		t.Fatalf("added = %v, err = %v", added, err)
	}

	owned, err := Blocks(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, lines := range owned {
		for _, l := range lines {
			if l.Host() == "operator.test" {
				t.Errorf("the operator's line landed inside %q's block, where an upgrade would erase it", name)
			}
		}
	}
	mine, err := Unmanaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Host() != "operator.test" {
		t.Errorf("unmanaged = %#v, want the one line just added", mine)
	}

	// And it survives what it is supposed to survive.
	if _, err := Remove(path, "acme"); err != nil {
		t.Fatal(err)
	}
	if mine, _ = Unmanaged(path); len(mine) != 1 {
		t.Error("removing the provider took the operator's line with it")
	}
}

// A host of 24 characters or more used to run into its methods, because the
// column pad is a minimum width rather than a separator. The result parses as
// one field, so the mangled name is what gets permitted: the destination the
// operator asked for stays blocked while the file looks like it was granted,
// `allowlist deny` cannot match the host to take it back, and Allow's own
// idempotence check misses it and appends a duplicate on every call.
func TestAllowSeparatesALongHostFromItsMethods(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	const host = "a-very-long-destination.test" // 28 characters, past the pad
	if added, err := Allow(path, host, "GET,POST"); err != nil || !added {
		t.Fatalf("added = %v, err = %v", added, err)
	}

	mine, err := Unmanaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Host() != host {
		t.Fatalf("read back %#v, want the one line for %s", mine, host)
	}

	// The consequences, each of which the run-together line broke.
	if added, _ := Allow(path, host, "GET"); added {
		t.Error("a second Allow for the same host reported a change")
	}
	if mine, _ := Unmanaged(path); len(mine) != 1 {
		t.Errorf("the host was permitted twice: %#v", mine)
	}
	if removed, err := Deny(path, host); err != nil || !removed {
		t.Errorf("Deny could not take back what Allow wrote: removed = %v, err = %v", removed, err)
	}
}

// Below the threshold the column layout is what the bank's own allowlist files
// use, and the fix above must not have shifted it.
func TestAllowKeepsTheMethodsColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	if _, err := Allow(path, "api.acme.test", "GET,POST"); err != nil {
		t.Fatal(err)
	}
	want := "api.acme.test           GET,POST"
	if got := read(t, path); !strings.Contains(got, want) {
		t.Errorf("allowlist =\n%s\nwant a line %q", got, want)
	}
}

func TestAllowIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	if added, _ := Allow(path, "operator.test", "*"); !added {
		t.Fatal("first Allow reported no change")
	}
	added, err := Allow(path, "operator.test", "GET")
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("a second Allow for the same host reported a change")
	}
	if mine, _ := Unmanaged(path); len(mine) != 1 {
		t.Errorf("the host was permitted twice: %#v", mine)
	}
}

// Deleting an entry's line would work until the next add, upgrade or reset put
// it back. A grant that reappears with nothing to explain it is worse than one
// that was never removed, so this refuses and names the entry.
func TestDenyRefusesADestinationAnEntryOwns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test POST"}}); err != nil {
		t.Fatal(err)
	}

	_, err := Deny(path, "api.acme.test")
	var managed *ErrManaged
	if err == nil {
		t.Fatal("a managed destination was removed")
	}
	if !errorsAs(err, &managed) || managed.Owner != "acme" {
		t.Fatalf("err = %v, want ErrManaged naming acme", err)
	}

	// And nothing was written on the way to refusing.
	owned, _ := Blocks(path)
	if len(owned["acme"]) != 1 {
		t.Error("the block was modified by a call that refused")
	}
}

func errorsAs(err error, target **ErrManaged) bool {
	e, ok := err.(*ErrManaged)
	if ok {
		*target = e
	}
	return ok
}
