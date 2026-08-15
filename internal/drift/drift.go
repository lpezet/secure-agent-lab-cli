// Package drift compares what a deployment holds against what its pinned
// release ships.
//
// This is the problem the whole repo exists for, stated as a check. A
// deployment's compose file builds each image from the stack at its pinned
// tag, but the files that ENFORCE the boundary — proxy/*.py, broker/*.js,
// cred-gateway/*.conf — are bind-mounted from the deployment's own
// directories. Repinning moves the images and leaves those files byte for
// byte as they were, with nothing in `docker compose ps` to show for it. So a
// lab can sit on a release containing a security fix and keep running the
// vulnerable file.
//
// scripts/check-drift.sh in the stack repo answers the same question for a
// deployment sal has never touched, and it stays over there: dependency-free
// bash that works for someone who never installs this CLI. What is different
// here is the evidence available. That script has to GUESS which example a
// deployment came from and match files by name; a sal-managed deployment
// records what was installed, from which release, at which commit — so this
// compares against a known answer rather than a likely one.
package drift

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is what a finding says about one file.
type Kind string

const (
	// OK: the file matches what the pinned release ships.
	OK Kind = "ok"

	// Drift: the file exists and differs. THE finding — a boundary file that
	// is not the one the release it claims to be at ships.
	Drift Kind = "DRIFT"

	// Missing: the release ships it, and the deployment does not have it. A
	// control that is absent rather than stale, which is worse: 000_policy.py
	// missing means the proxy has no internal-host block at all.
	Missing Kind = "MISSING"

	// Stale: recorded as installed, still on disk, and no longer shipped at
	// this release. A cred-gateway config in this state keeps whitelisting a
	// route its entry no longer exposes, which is a widened boundary nothing
	// else would report.
	Stale Kind = "STALE"

	// Unowned: present in a directory sal manages, and nothing accounts for
	// it. See Check for why this is a finding here and only a note in the
	// stack's script.
	Unowned Kind = "UNOWNED"

	// Note: worth saying, and not a finding.
	Note Kind = "note"
)

// Failing reports whether a kind means the deployment is not what it claims.
func (k Kind) Failing() bool {
	switch k {
	case Drift, Missing, Stale, Unowned:
		return true
	}
	return false
}

// Expected is one file the deployment should hold, and where the copy to
// compare against lives.
type Expected struct {
	// Path is relative to the deployment.
	Path string

	// Src is the reference copy, absolute. Empty means the pinned release no
	// longer ships this file, which makes an on-disk copy Stale.
	Src string

	// Owner says where the reference came from — "bank/acme/proxy/" — and is
	// printed, so a reader can go and look at it.
	Owner string
}

// Finding is one line of the report.
type Finding struct {
	Kind   Kind
	Path   string
	Detail string

	// Src is kept so --show-diff has something to diff against, and is empty
	// for findings that are not a comparison.
	Src string

	// Ref is the reference content when it is not a file on disk. The
	// generated compose file is compared against a freshly rendered template,
	// so there is nothing for Src to point at — and that file is worth being
	// able to diff, since the loopback-only observer publish and the internal
	// lab network both live in it.
	Ref []byte
}

// Report is every finding, in the order they should be read.
type Report struct {
	Findings []Finding
}

// Add appends a finding. Exported because a caller compares things this
// package cannot see — the generated compose file, whose reference is a
// rendered template rather than a file in the release.
func (r *Report) Add(f Finding) { r.Findings = append(r.Findings, f) }

// Count returns how many findings are of a kind.
func (r *Report) Count(k Kind) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// Failed reports whether anything found means the deployment is not what it
// claims to be.
func (r *Report) Failed() bool {
	for _, f := range r.Findings {
		if f.Kind.Failing() {
			return true
		}
	}
	return false
}

// ManagedDirs are the deployment directories whose contents sal decides.
//
// These are exactly the three the compose file bind-mounts into the services
// that enforce the boundary. `lab/` is deliberately NOT here: its Dockerfile
// is the operator's own build context, so scanning it would report their file
// as unowned. A lab_setup fragment inside it is still compared, because it
// arrives as an Expected like any other installed file — the difference is
// between comparing what we know about and judging what we do not.
var ManagedDirs = []string{"broker", "proxy", "cred-gateway"}

// Check compares a deployment against the files its pinned release ships.
//
// An unowned file is a finding here, where the stack's script calls the same
// thing `custom` and passes it. The difference is what a record means: in a
// hand-rolled deployment a file with no upstream counterpart is ordinary,
// because the deployment was assembled by hand. In a sal-managed one every
// boundary file arrived through `sal init` or `sal providers add` and was
// written down, so a file none of them accounts for arrived some other way —
// and the thing most likely to have put an extra .conf in cred-gateway/ is
// the agent the boundary exists to contain.
//
// alsoOwned names files whose owner is known but whose reference could not be
// resolved — an entry recorded in installed.json that this release cannot
// produce. They are not compared, because there is nothing to compare against,
// and they must not be reported as unowned either: sal put them there, and the
// caller says so once about the entry rather than four times about its files.
func Check(deployDir string, expected []Expected, alsoOwned []string) (*Report, error) {
	r := &Report{}

	owned := make(map[string]bool, len(expected)+len(alsoOwned))
	for _, e := range expected {
		owned[filepath.Clean(e.Path)] = true
	}
	for _, p := range alsoOwned {
		owned[filepath.Clean(p)] = true
	}

	sorted := append([]Expected(nil), expected...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	for _, e := range sorted {
		local := filepath.Join(deployDir, filepath.FromSlash(e.Path))
		localBytes, err := os.ReadFile(local)

		switch {
		case e.Src == "":
			// No reference: the release does not ship this any more.
			if err != nil {
				// Already gone, so there is nothing left to be stale.
				continue
			}
			r.Add(Finding{Kind: Stale, Path: e.Path,
				Detail: "recorded, but not shipped at this release — `sal upgrade` deletes it"})

		case err != nil && os.IsNotExist(err):
			r.Add(Finding{Kind: Missing, Path: e.Path, Src: e.Src,
				Detail: "shipped at this release and not in this deployment"})

		case err != nil:
			return nil, err

		default:
			refBytes, err := os.ReadFile(e.Src)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(localBytes, refBytes) {
				r.Add(Finding{Kind: OK, Path: e.Path, Src: e.Src, Detail: "matches " + e.Owner})
			} else {
				r.Add(Finding{Kind: Drift, Path: e.Path, Src: e.Src, Detail: "differs from " + e.Owner})
			}
		}
	}

	unowned, err := unownedFiles(deployDir, owned)
	if err != nil {
		return nil, err
	}
	for _, p := range unowned {
		r.Add(Finding{Kind: Unowned, Path: p,
			Detail: "nothing installed this, so nothing updates it"})
	}

	return r, nil
}

// unownedFiles lists files under the managed directories that no Expected
// accounts for.
func unownedFiles(deployDir string, owned map[string]bool) ([]string, error) {
	var found []string
	for _, dir := range ManagedDirs {
		items, err := os.ReadDir(filepath.Join(deployDir, dir))
		if err != nil {
			// A managed directory that is absent is reported through the
			// files that should have been in it, not twice.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, it := range items {
			if it.IsDir() {
				continue
			}
			rel := dir + "/" + it.Name()
			if owned[filepath.Clean(rel)] {
				continue
			}
			found = append(found, rel)
		}
	}
	sort.Strings(found)
	return found, nil
}

// Diff renders a line diff of two files, reference first.
//
// Deliberately simple: it trims the common prefix and suffix and prints what
// is between them. That is a correct diff and not a minimal one — an edit in
// the middle of a file shows more context than `diff` would — and it costs no
// dependency and cannot be subtly wrong. What a reader needs from this is
// which lines to go and look at.
func Diff(refPath, localPath string) (string, error) {
	ref, err := os.ReadFile(refPath)
	if err != nil {
		return "", err
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}
	return DiffBytes(ref, local), nil
}

// DiffBytes is Diff for content that is not on disk.
func DiffBytes(refContent, localContent []byte) string {
	ref, local := lines(refContent), lines(localContent)

	head := 0
	for head < len(ref) && head < len(local) && ref[head] == local[head] {
		head++
	}
	tail := 0
	for tail < len(ref)-head && tail < len(local)-head &&
		ref[len(ref)-1-tail] == local[len(local)-1-tail] {
		tail++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "@@ line %d @@\n", head+1)
	for _, line := range ref[head : len(ref)-tail] {
		fmt.Fprintf(&b, "-%s\n", line)
	}
	for _, line := range local[head : len(local)-tail] {
		fmt.Fprintf(&b, "+%s\n", line)
	}
	return b.String()
}

func lines(b []byte) []string {
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
