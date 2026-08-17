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

	var b strings.Builder
	b.WriteString(strings.TrimRight(kept, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(begin(name))
	b.WriteString("\n")
	for _, l := range lines {
		b.WriteString(l.Text)
		if l.Why != "" {
			b.WriteString("   # " + l.Why)
		}
		b.WriteString("\n")
	}
	b.WriteString(end(name))
	b.WriteString("\n")

	return lines, write(path, b.String())
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

// Allow adds a destination to the operator's own lines, outside every block.
//
// Outside deliberately: a line added here survives `providers remove` and is
// never rewritten by an upgrade, which is what someone typing it means. Adding
// INTO a block would produce a grant that vanishes the next time the entry is
// reinstalled, with nothing to say why.
//
// Reports whether it changed anything, so a caller can say "already permitted"
// rather than implying it did something.
func Allow(path, host, methods string) (added bool, err error) {
	text := host
	if methods != "" {
		text = fmt.Sprintf("%-24s%s", host, methods)
	}

	existing, err := Unmanaged(path)
	if err != nil {
		return false, err
	}
	for _, l := range existing {
		if l.Host() == host {
			return false, nil
		}
	}

	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}

	// Before the first block, so the operator's own policy stays together at
	// the top rather than being interleaved with whatever was installed last.
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	at := len(lines)
	for i, raw := range lines {
		if _, ok := blockName(strings.TrimSpace(raw), "# --- sal:"); ok {
			at = i
			break
		}
	}
	// Trim blank lines back from the insertion point so repeated calls do not
	// accumulate gaps.
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}

	out := append([]string{}, lines[:at]...)
	out = append(out, text)
	if at < len(lines) {
		out = append(out, "")
	}
	out = append(out, lines[at:]...)
	return true, write(path, strings.Join(out, "\n"))
}

// ErrManaged means the destination belongs to an installed entry, so removing
// it here would be undone the next time that entry is written.
type ErrManaged struct {
	Host, Owner string
}

func (e *ErrManaged) Error() string {
	return e.Host + " is permitted by the " + e.Owner + " entry"
}

// Deny removes one of the operator's own destinations.
//
// It refuses a line inside a block rather than deleting it. Deleting would
// work until the next `providers add`, `upgrade` or `allowlist reset` put it
// back — a grant that reappears with nothing to explain it is worse than one
// that was never removed, and the honest answer is `sal providers remove`.
func Deny(path, host string) (removed bool, err error) {
	owned, err := Blocks(path)
	if err != nil {
		return false, err
	}
	for name, lines := range owned {
		for _, l := range lines {
			if l.Host() == host {
				return false, &ErrManaged{Host: host, Owner: name}
			}
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var out []string
	inside := false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if _, ok := blockName(line, "# --- sal:"); ok {
			inside = true
		} else if _, ok := blockName(line, "# --- end sal:"); ok {
			inside = false
		} else if !inside && line != "" && !strings.HasPrefix(line, "#") {
			if l, ok := parseLine(line); ok && l.Host() == host {
				removed = true
				continue
			}
		}
		out = append(out, raw)
	}
	if !removed {
		return false, nil
	}
	return true, write(path, strings.Join(out, "\n"))
}
