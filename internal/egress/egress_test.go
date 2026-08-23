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

	g, err := Allow(path, "operator.test", "*")
	if err != nil || !g.Wrote {
		t.Fatalf("wrote = %v, err = %v", g.Wrote, err)
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
	if g, err := Allow(path, host, "GET,POST"); err != nil || !g.Wrote {
		t.Fatalf("wrote = %v, err = %v", g.Wrote, err)
	}

	mine, err := Unmanaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Host() != host {
		t.Fatalf("read back %#v, want the one line for %s", mine, host)
	}

	// The consequences, each of which the run-together line broke.
	if g, _ := Allow(path, host, "GET,POST"); g.Wrote {
		t.Error("a second Allow for the same host reported a change")
	}
	if mine, _ := Unmanaged(path); len(mine) != 1 {
		t.Errorf("the host was permitted twice: %#v", mine)
	}
	if rev, err := Deny(path, host); err != nil || !rev.Removed {
		t.Errorf("Deny could not take back what Allow wrote: removed = %v, err = %v", rev.Removed, err)
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

	if g, _ := Allow(path, "operator.test", "*"); !g.Wrote {
		t.Fatal("first Allow reported no change")
	}
	g, err := Allow(path, "operator.test", "*")
	if err != nil {
		t.Fatal(err)
	}
	if g.Wrote {
		t.Error("a second Allow for the same host reported a change")
	}
	if mine, _ := Unmanaged(path); len(mine) != 1 {
		t.Errorf("the host was permitted twice: %#v", mine)
	}
}

// The same call with DIFFERENT methods is not the same call. It used to report
// "already permitted" and change nothing, which is issue #50's failure shape
// one step earlier: the operator asks to widen a destination, sal says it is
// permitted, and the methods they asked for are not in the file at all.
func TestAllowRewritesYourOwnLineWhenTheMethodsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	if _, err := Allow(path, "operator.test", "GET"); err != nil {
		t.Fatal(err)
	}
	g, err := Allow(path, "operator.test", "GET,POST")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Wrote || g.Replaced == "" {
		t.Fatalf("wrote = %v, replaced = %q, want the previous line reported", g.Wrote, g.Replaced)
	}

	mine, _ := Unmanaged(path)
	if len(mine) != 1 {
		t.Fatalf("the host was permitted twice: %#v", mine)
	}
	if !strings.Contains(mine[0].Text, "GET,POST") {
		t.Errorf("line = %q, want the methods just asked for", mine[0].Text)
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
	var managed *ErrManaged // nolint: the assertion is below
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

// The ordering rule, stated as the property the proxy actually enforces.
//
// stack/proxy/addons/001_allowlist.py loads the file with
// `entries[domain] = methods`, so two lines for one destination are one dict
// entry and the LAST of them is the rule. Issue #50: an operator widening what
// an entry seeded got their line written above the entry's block, sal reported
// the destination permitted with the methods asked for, and the proxy went on
// enforcing the entry's narrower line. Position is enforcement in this file,
// so the operator's lines go last.
func TestTheOperatorsLineComesAfterEveryBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Allow(path, "api.acme.test", "GET,POST"); err != nil {
		t.Fatal(err)
	}

	assertLast(t, path, "api.acme.test", "GET,POST")

	// And the next install, upgrade or reset must not put the entry back in
	// front of it — the same bug with the two writers swapped.
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}
	assertLast(t, path, "api.acme.test", "GET,POST")

	// A second entry arriving later must not either.
	if _, err := Write(path, "beta", []Line{{Text: "api.beta.test GET"}}); err != nil {
		t.Fatal(err)
	}
	assertLast(t, path, "api.acme.test", "GET,POST")
}

// A lab written by an older sal has the operator's line ABOVE the block, where
// it does nothing. Allow moves the one line it was asked about, and reports
// that it did — sal never silently reorders the rest of what somebody wrote.
func TestAllowMovesALineTheProxyWasIgnoring(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	legacy := `api.acme.test           GET,POST

# --- sal:acme --- managed; ` + "`sal providers remove acme`" + ` removes it
api.acme.test GET
# --- end sal:acme ---
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := Allow(path, "api.acme.test", "GET,POST")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Wrote || !g.Moved {
		t.Fatalf("wrote = %v, moved = %v, want the line reported as moved", g.Wrote, g.Moved)
	}
	if mine, _ := Unmanaged(path); len(mine) != 1 {
		t.Fatalf("the line was duplicated rather than moved: %#v", mine)
	}
	assertLast(t, path, "api.acme.test", "GET,POST")
}

// Widening an entry's grant makes that entry's line inert, which is a change
// to what the boundary enforces. The caller reports it; this is what gives it
// the words.
func TestAllowNamesTheEntryLineItDisplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}

	g, err := Allow(path, "api.acme.test", "GET,POST")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Shadowed) != 1 || g.Shadowed[0].Entry != "acme" {
		t.Fatalf("shadowed = %#v, want the acme line", g.Shadowed)
	}
	if g.Shadowed[0].Line.Text != "api.acme.test GET" {
		t.Errorf("shadowed line = %q, want it reported as it stands in the file", g.Shadowed[0].Line.Text)
	}

	// A destination nothing else names displaces nothing, and must not be
	// reported as if it had.
	g, err = Allow(path, "other.test", "GET")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Shadowed) != 0 {
		t.Errorf("shadowed = %#v for a destination no entry names", g.Shadowed)
	}
}

// Effective is the only thing that can say which of two lines for one host is
// real, and the reason `allowlist list` can be trusted after #50.
func TestEffectiveNamesTheLineTheProxyKeeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}, {Text: "cdn.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Allow(path, "api.acme.test", "GET,POST"); err != nil {
		t.Fatal(err)
	}

	eff, err := Effective(path)
	if err != nil {
		t.Fatal(err)
	}
	if eff["api.acme.test"] != "" {
		t.Errorf("api.acme.test resolves to %q, want the operator's line", eff["api.acme.test"])
	}
	if eff["cdn.acme.test"] != "acme" {
		t.Errorf("cdn.acme.test resolves to %q, want the acme entry", eff["cdn.acme.test"])
	}
}

// A wildcard is NOT a collision, and sal must not report one. hostmatch.find
// gives exact over wildcard and a longer suffix over a shorter one, whatever
// the file's order — so nothing here decides between two different patterns,
// and claiming a wildcard shadows a host would be sal inventing a rule the
// proxy does not have.
func TestAWildcardIsNotACollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}

	g, err := Allow(path, "*.acme.test", "GET,POST")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Shadowed) != 0 {
		t.Errorf("shadowed = %#v, want nothing: a wildcard and a host are different patterns", g.Shadowed)
	}
	eff, _ := Effective(path)
	if eff["api.acme.test"] != "acme" {
		t.Errorf("api.acme.test resolves to %q; the wildcard does not displace it", eff["api.acme.test"])
	}
}

// Case is not a second destination: the proxy lowercases the key, so two
// spellings are one grant to it and must be one to sal.
func TestHostCaseIsTheProxysRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")

	if _, err := Allow(path, "API.Acme.Test", "GET"); err != nil {
		t.Fatal(err)
	}
	if g, _ := Allow(path, "api.acme.test", "GET"); g.Wrote {
		t.Error("the same destination in another case was permitted twice")
	}
	if rev, err := Deny(path, "API.ACME.TEST"); err != nil || !rev.Removed {
		t.Errorf("Deny could not match the host it wrote: removed = %v, err = %v", rev.Removed, err)
	}
}

// Denying a line that widened an entry's does not close the destination — it
// hands it back. Refusing the removal instead would leave `allow` with no
// counterpart: take an entry's grant and never give it back.
func TestDenyTakesYourLineAndSaysWhatIsLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	if _, err := Write(path, "acme", []Line{{Text: "api.acme.test GET"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Allow(path, "api.acme.test", "GET,POST"); err != nil {
		t.Fatal(err)
	}

	rev, err := Deny(path, "api.acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if !rev.Removed {
		t.Fatal("the operator's own line was not removed")
	}
	if len(rev.Restored) != 1 || rev.Restored[0].Entry != "acme" {
		t.Fatalf("restored = %#v, want the acme line reported as still permitting it", rev.Restored)
	}
	if mine, _ := Unmanaged(path); len(mine) != 0 {
		t.Errorf("unmanaged = %#v, want nothing left of yours", mine)
	}
	if owned, _ := Blocks(path); len(owned["acme"]) != 1 {
		t.Error("denying your own line reached into the entry's block")
	}
}

// Appending at the end of the file meets one shape where the end of the file
// is inside somebody's block. A line written there would be deleted by the
// next `providers remove` — the one place the "outside every block" rule could
// break silently.
func TestAllowNeverWritesIntoAnUnterminatedBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist")
	broken := "# --- sal:acme --- managed\napi.acme.test GET\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Allow(path, "operator.test", "*"); err != nil {
		t.Fatal(err)
	}
	mine, err := Unmanaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Host() != "operator.test" {
		t.Fatalf("unmanaged = %#v, want the line just added, outside the block", mine)
	}
	if _, err := Remove(path, "acme"); err != nil {
		t.Fatal(err)
	}
	if mine, _ = Unmanaged(path); len(mine) != 1 {
		t.Error("removing the entry took the operator's line with it")
	}
}

// assertLast is the ordering property itself: of every line naming this
// destination, the one the proxy keeps is the operator's.
func assertLast(t *testing.T, path, host, methods string) {
	t.Helper()

	last := ""
	for _, raw := range strings.Split(read(t, path), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 && f[0] == host {
			last = line
		}
	}
	if last == "" {
		t.Fatalf("no line for %s in\n%s", host, read(t, path))
	}
	if !strings.Contains(last, methods) {
		t.Errorf("the last line for %s is %q, want the one permitting %s\n%s", host, last, methods, read(t, path))
	}
}
