// Package egress merges a bank entry's declared egress into a deployment's
// allowlist.
//
// Installing an entry used to produce a lab that could not use it: the entry
// brought its broker provider, its addon and its credential wiring, and then
// every request it made was denied, because the allowlist is the operator's
// and nothing seeded it. Working it out by hand is worse than it looks —
// `hosts` carries no methods and the proxy defaults a line with none to
// GET,HEAD,OPTIONS, so a bare `api.anthropic.com` reads as correct and blocks
// every POST. Stack 1.13.0 answers it: an entry ships `allowlist`, in the
// allowlist's own syntax, with anything optional commented out.
//
// Two rules shape everything here:
//
//   - Only what the entry left UNCOMMENTED is ever written. A commented line
//     is a suggestion, and turning it on is the operator's to type. That is
//     what makes seeding safe to do by default: it grants exactly the egress
//     the entry says it needs to function, and nothing a vendor would like.
//   - What sal writes lives in a MARKED BLOCK, and everything outside every
//     block belongs to the operator and is never touched. Without that,
//     removing a provider could not tell its own line from a hand-written one,
//     and would have to choose between leaving egress open and deleting
//     something somebody meant.
package egress

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Line is one allowlist entry: a destination and the methods permitted to it.
//
// Kept as text rather than parsed into a host and a method set, because sal is
// not the thing that enforces this file — the proxy is. Re-rendering a line
// from a parse would let sal's understanding of the syntax drift away from the
// addon's, and the addon's is the one that decides what actually leaves.
type Line struct {
	Text string // the entry as written, minus any comment marker
	Why  string // trailing `# ...` on the same line, if the entry explained itself
}

// Host is the first field, for reporting. Never used to decide anything.
func (l Line) Host() string {
	f := strings.Fields(l.Text)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Entry is what a bank entry declares it needs to reach.
type Entry struct {
	Enabled  []Line // uncommented: written on install
	Optional []Line // commented out below the OPTIONAL marker: reported, never written
}

// optionalMarker is how a bank entry separates what it needs from what it
// merely offers. Matched loosely — the shipped files draw a full comment rule
// around the word — because the consequence of missing it is that a suggestion
// is reported as if it were nowhere, and the consequence of over-matching is
// nothing at all. Neither can widen egress: only uncommented lines are ever
// written, whichever side of the marker they fall on.
const optionalMarker = "OPTIONAL"

// Parse reads an entry's allowlist file.
func Parse(body []byte) Entry {
	var e Entry
	optional := false

	s := bufio.NewScanner(bytes.NewReader(body))
	for s.Scan() {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			continue
		}
		if !strings.HasPrefix(raw, "#") {
			if line, ok := parseLine(raw); ok {
				e.Enabled = append(e.Enabled, line)
			}
			continue
		}

		bare := strings.TrimSpace(strings.TrimLeft(raw, "#"))
		if strings.Contains(bare, optionalMarker) {
			optional = true
			continue
		}
		// A commented line below the marker MAY be a suggested entry, or may
		// be prose explaining one — the shipped files carry plenty of both.
		// Telling them apart is a guess, and it is only ever used to print
		// "available and off". Guessing wrong makes a listing noisier or
		// shorter; it cannot grant anything.
		if optional {
			if line, ok := parseLine(bare); ok && looksLikeDestination(line.Host()) {
				e.Optional = append(e.Optional, line)
			}
		}
	}
	return e
}

// parseLine splits an entry from the comment that explains it.
func parseLine(s string) (Line, bool) {
	text, why := s, ""
	if i := strings.Index(s, "#"); i >= 0 {
		text = strings.TrimSpace(s[:i])
		why = strings.TrimSpace(s[i+1:])
	}
	if text == "" {
		return Line{}, false
	}
	return Line{Text: text, Why: why}, true
}

// looksLikeDestination is the guess described in Parse, kept deliberately dumb:
// one token, and it resembles a hostname. Prose fails it because prose has
// spaces.
func looksLikeDestination(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t") {
		return false
	}
	if !strings.Contains(host, ".") {
		return false
	}
	return !strings.ContainsAny(host, "`'\"(),:;")
}

// begin and end delimit what sal owns for one entry.
func begin(name string) string {
	return "# --- sal:" + name + " --- managed; `sal providers remove " + name + "` removes it"
}

func end(name string) string { return "# --- end sal:" + name + " ---" }

// Write puts an entry's enabled lines into the allowlist, replacing any block
// already there for that name.
//
// Returns what it wrote. Callers report that: seeding widens egress, so it is
// never something an operator finds out about later.
func Write(path, name string, lines []Line) ([]Line, error) {
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	kept, _ := split(string(body), name)
	if len(lines) == 0 {
		// Nothing to add, and any previous block for this name goes: an entry
		// that stopped needing a destination should stop permitting it.
		return nil, write(path, kept)
	}

	block := []string{begin(name)}
	for _, l := range lines {
		text := l.Text
		if l.Why != "" {
			text += "   # " + l.Why
		}
		block = append(block, text)
	}
	block = append(block, end(name))

	// ABOVE the operator's own lines, rather than appended to the end of the
	// file. The proxy keeps the LAST line for a destination (see hostKey), so a
	// block written below a line somebody typed silently takes that line's
	// place — which is issue #50 with the two writers swapped. Fixing only
	// Allow would have left every `providers add`, `upgrade` and `allowlist
	// reset` putting the entry back in front of the operator afterwards.
	existing := splitLines(kept)
	out := spliceBlock(existing, blockInsertion(existing), block)
	return lines, write(path, strings.Join(out, "\n"))
}

// Remove deletes the block for one entry and leaves everything else exactly as
// it was. Reports what it removed, and whether there was a block at all.
func Remove(path, name string) ([]Line, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	kept, removed := split(string(body), name)
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, write(path, kept)
}

// Blocks reports what each entry currently owns in this allowlist, so a
// listing can say which line came from where.
func Blocks(path string) (map[string][]Line, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	owned := map[string][]Line{}
	var current string
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if name, ok := blockName(line, "# --- sal:"); ok {
			current = name
			owned[current] = nil
			continue
		}
		if _, ok := blockName(line, "# --- end sal:"); ok {
			current = ""
			continue
		}
		if current == "" || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if l, ok := parseLine(line); ok {
			owned[current] = append(owned[current], l)
		}
	}
	return owned, nil
}

func blockName(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	name, _, found := strings.Cut(rest, " ")
	if !found {
		name = strings.TrimSuffix(rest, "---")
	}
	return strings.TrimSpace(name), true
}

// split separates the file into everything that is not this entry's block, and
// the entries inside the block that was there.
//
// An unterminated block — someone deleted the end marker — consumes to the end
// of the file rather than being ignored. The alternative is treating the
// remaining lines as the operator's and leaving a stale grant in place, and
// between the two, a removal that takes too much is visible while one that
// leaves egress open is not.
func split(body, name string) (kept string, removed []Line) {
	var out []string
	inside := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if n, ok := blockName(line, "# --- sal:"); ok && n == name {
			inside = true
			continue
		}
		if n, ok := blockName(line, "# --- end sal:"); ok && n == name {
			inside = false
			continue
		}
		if inside {
			if l, ok := parseLine(line); ok && !strings.HasPrefix(line, "#") {
				removed = append(removed, l)
			}
			continue
		}
		out = append(out, raw)
	}
	return strings.Join(out, "\n"), removed
}

func write(path, body string) error {
	body = strings.TrimRight(body, "\n") + "\n"
	return os.WriteFile(path, []byte(body), 0o600)
}

// Describe renders one line for an operator, host first.
func Describe(l Line) string {
	if l.Why == "" {
		return l.Text
	}
	return fmt.Sprintf("%-38s (%s)", l.Text, l.Why)
}

// Unmanaged returns the lines the operator wrote themselves — everything that
// is not inside any sal block.
//
// The distinction is the whole reason blocks exist, and it is what `sal
// allowlist list` is for: "which of these did I decide, and which arrived with
// a provider" is not answerable from the file by eye once there are three
// entries and a few hand-written lines.
func Unmanaged(path string) ([]Line, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []Line
	inside := false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if _, ok := blockName(line, "# --- sal:"); ok {
			inside = true
			continue
		}
		if _, ok := blockName(line, "# --- end sal:"); ok {
			inside = false
			continue
		}
		if inside || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if l, ok := parseLine(line); ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// Managed is one line inside an entry's block, named by the entry that owns it.
type Managed struct {
	Entry string
	Line  Line
}

// Grant is what Allow did, and what it now takes precedence over.
type Grant struct {
	Host     string    // the destination, as written into the file
	Text     string    // the whole line written, when one was
	Wrote    bool      // false means the file already said exactly this
	Moved    bool      // your line was already there; only its position changed
	Replaced string    // the previous text of your line, when its methods changed
	Shadowed []Managed // entry lines for the same destination, now inert
}

// Allow adds a destination to the operator's own lines, outside every block and
// AFTER all of them.
//
// Outside deliberately: a line added here survives `providers remove` and is
// never rewritten by an upgrade, which is what someone typing it means. Adding
// INTO a block would produce a grant that vanishes the next time the entry is
// reinstalled, with nothing to say why.
//
// After them for a different reason, and it is issue #50. This used to insert
// before the first block, so that "the operator's own policy stays together at
// the top" — a legibility argument, applied to a file where position is
// enforcement. An operator widening what an entry seeded (`platform.claude.com
// GET` -> `GET,POST`, so `claude auth login` can POST its token) got a line
// written above the entry's, sal reporting the destination permitted with the
// methods asked for, `allowlist list` showing it, and the POST still refused —
// because the entry's narrower line came later and replaced it in the proxy's
// dict. Last-wins makes the file's tail the operator's final word, which is
// the intuition anyway.
//
// Reports what happened rather than a bool, because "already permitted" and
// "permitted, and it now overrides the anthropic entry" are different things
// for the caller to say, and the second is the one nobody would otherwise know.
func Allow(path, host, methods string) (Grant, error) {
	// Lowercased because that is the key the proxy stores the line under, so
	// two spellings of one host are one grant to it and must be one to sal.
	g := Grant{Host: hostKey(host)}

	text := g.Host
	if methods != "" {
		// The pad is a MINIMUM, so the separating space has to be its own
		// character: `%-24s%s` emits nothing for a host of 24 or more and runs
		// the two fields together. That parses — as one field — so the mangled
		// name becomes the permitted destination, the real one stays blocked,
		// and neither `allowlist deny` nor Allow's own idempotence check can
		// find the host again. Padding to 23 with an explicit space is
		// byte-identical below the threshold and correct above it.
		text = fmt.Sprintf("%-23s %s", g.Host, methods)
	}
	g.Text = text

	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Grant{}, err
	}
	lines := splitLines(string(body))

	mine, last := -1, -1
	var mineLine Line
	inside := ""
	for i, raw := range lines {
		s := strings.TrimSpace(raw)
		if n, ok := blockName(s, "# --- sal:"); ok {
			inside = n
			continue
		}
		if _, ok := blockName(s, "# --- end sal:"); ok {
			inside = ""
			continue
		}
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		l, ok := parseLine(s)
		if !ok || hostKey(l.Host()) != g.Host {
			continue
		}
		last = i
		if inside == "" {
			mine, mineLine = i, l
		} else {
			g.Shadowed = append(g.Shadowed, Managed{Entry: inside, Line: l})
		}
	}

	// Already exactly this, and already the line the proxy would keep.
	if mine >= 0 && mineLine.Text == text && last == mine {
		return g, nil
	}
	if mine >= 0 {
		if mineLine.Text == text {
			// Same grant, wrong side of a block — the lab this was written for.
			// Repairing it here rather than in Write is the narrow choice: sal
			// moves the one line the operator just named, and never reorders
			// the rest of what they wrote.
			g.Moved = true
		} else {
			g.Replaced = mineLine.Text
		}
	}
	g.Wrote = true

	out := make([]string, 0, len(lines)+2)
	for i, raw := range lines {
		if i == mine {
			continue
		}
		out = append(out, raw)
	}
	out = closeUnterminated(out)
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && commentOnly(out[len(out)-1]) {
		out = append(out, "")
	}
	out = append(out, text)
	return g, write(path, strings.Join(out, "\n"))
}

// hostKey is how the proxy keys a line, and it is the one thing that decides
// whether two lines are about the same destination.
//
// Restated from the stack's 001_allowlist.py, which loads the file into
// `entries[domain] = methods` with `domain` the lowercased first field. Two
// lines with the same key are one dict entry, so the LAST of them is what the
// proxy enforces and every earlier one is inert. That rule is not
// machine-readable from anywhere — it is the shape of a Python assignment — so
// this is the second place in sal, after the load_band NNN ranges, that can
// silently desync from the stack. If a release ever makes the file first-wins,
// or merges the methods of duplicate lines, this comment and Effective are
// what have to move.
//
// Note how narrow it is. Selection between DIFFERENT patterns is not file order
// at all: hostmatch.find gives exact over wildcard and a longer wildcard suffix
// over a shorter one, explicitly so that ordering a security decision by
// however a config file happened to be written cannot happen. So
// `*.example.test` never shadows `api.example.test`, and sal must not say it
// does. Only an identical key collides.
func hostKey(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

// Effective reports who owns the line the proxy will actually enforce for each
// destination: the entry's name, or "" for one the operator wrote themselves.
//
// Blocks and Unmanaged both discard position, so neither can answer this, and
// `sal allowlist list` groups by who decided each line — which without this
// prints the same host under two headings and cannot say which one is in
// force. That is the silent case in issue #50: a grant that is in the file, is
// listed, and does nothing.
func Effective(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	owner := map[string]string{}
	inside := ""
	for _, raw := range strings.Split(string(body), "\n") {
		s := strings.TrimSpace(raw)
		if n, ok := blockName(s, "# --- sal:"); ok {
			inside = n
			continue
		}
		if _, ok := blockName(s, "# --- end sal:"); ok {
			inside = ""
			continue
		}
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if l, ok := parseLine(s); ok {
			// Plain assignment in file order, exactly as the addon does it.
			owner[hostKey(l.Host())] = inside
		}
	}
	return owner, nil
}

// splitLines reads a file body as lines, with an empty body meaning no lines
// rather than one blank one.
func splitLines(body string) []string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func commentOnly(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// blockInsertion is where a managed block goes: after every block already
// there, and before the first line the operator wrote.
//
// A comment run directly above that line comes with it, on the reading that a
// comment above an entry explains it. The file's own header is separated from
// anything below it by a blank line in every template the stack ships, so it
// stays at the top; a file that has been edited to remove that blank line gets
// the block above its header instead, which is cosmetic and visible.
func blockInsertion(lines []string) int {
	last := 0
	inside := false
	for i, raw := range lines {
		s := strings.TrimSpace(raw)
		if _, ok := blockName(s, "# --- sal:"); ok {
			inside, last = true, i+1
			continue
		}
		if _, ok := blockName(s, "# --- end sal:"); ok {
			inside, last = false, i+1
			continue
		}
		if inside {
			last = i + 1
			continue
		}
		if s == "" || commentOnly(raw) {
			continue
		}
		at := i
		for at > last && commentOnly(lines[at-1]) {
			at--
		}
		return at
	}
	return len(lines)
}

// spliceBlock puts a block at an index, with exactly one blank line on each
// side of it that has anything to separate from.
func spliceBlock(lines []string, at int, block []string) []string {
	head := append([]string{}, lines[:at]...)
	for len(head) > 0 && strings.TrimSpace(head[len(head)-1]) == "" {
		head = head[:len(head)-1]
	}
	tail := lines[at:]
	for len(tail) > 0 && strings.TrimSpace(tail[0]) == "" {
		tail = tail[1:]
	}

	out := head
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, block...)
	if len(tail) > 0 {
		out = append(out, "")
	}
	return append(out, tail...)
}

// closeUnterminated ends a block whose end marker somebody deleted, so that a
// line appended to the file lands outside it.
//
// split() already reads an unterminated block as running to the end of the
// file, on the grounds that a removal which takes too much is visible while one
// that leaves egress open is not. Writing the operator's line after it without
// this would put that line inside something `providers remove` deletes — the
// one place the whole "outside every block" rule is silently broken.
func closeUnterminated(lines []string) []string {
	open := ""
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if n, ok := blockName(s, "# --- sal:"); ok {
			open = n
			continue
		}
		if _, ok := blockName(s, "# --- end sal:"); ok {
			open = ""
		}
	}
	if open == "" {
		return lines
	}
	return append(lines, end(open))
}

// ErrManaged means the destination belongs to an installed entry, so removing
// it here would be undone the next time that entry is written.
type ErrManaged struct {
	Host, Owner string
}

func (e *ErrManaged) Error() string {
	return e.Host + " is permitted by the " + e.Owner + " entry"
}

// Revocation is what Deny took out, and what that leaves permitting the host.
type Revocation struct {
	Removed  bool
	Restored []Managed // entry lines for the same destination, back in force
}

// Deny removes one of the operator's own destinations.
//
// It refuses a line inside a block rather than deleting it. Deleting would
// work until the next `providers add`, `upgrade` or `allowlist reset` put it
// back — a grant that reappears with nothing to explain it is worse than one
// that was never removed, and the honest answer is `sal providers remove`.
//
// A host that has BOTH — an entry's line and one of yours widening it — is not
// that case, and refusing it would leave `allow` with no counterpart: the
// operator could take the entry's grant and never give it back. So the line
// that is theirs goes, and Restored says what is still permitting the host,
// because "denied" would otherwise be a lie about a destination the lab can
// still reach.
func Deny(path, host string) (Revocation, error) {
	key := hostKey(host)

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Revocation{}, nil
		}
		return Revocation{}, err
	}

	var rev Revocation
	var out []string
	inside := ""
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if n, ok := blockName(line, "# --- sal:"); ok {
			inside = n
		} else if _, ok := blockName(line, "# --- end sal:"); ok {
			inside = ""
		} else if line != "" && !strings.HasPrefix(line, "#") {
			if l, ok := parseLine(line); ok && hostKey(l.Host()) == key {
				if inside == "" {
					rev.Removed = true
					continue
				}
				rev.Restored = append(rev.Restored, Managed{Entry: inside, Line: l})
			}
		}
		out = append(out, raw)
	}

	if !rev.Removed {
		if len(rev.Restored) > 0 {
			return Revocation{}, &ErrManaged{Host: key, Owner: rev.Restored[0].Entry}
		}
		return Revocation{}, nil
	}
	return rev, write(path, strings.Join(out, "\n"))
}

// HostKey is the key rule above, for a caller outside this package.
//
// `sal allowlist list` has to look a line up in Effective's map, and doing that
// with its own strings.ToLower would be a second copy of the proxy's rule
// living somewhere it could disagree from.
func HostKey(host string) string { return hostKey(host) }
