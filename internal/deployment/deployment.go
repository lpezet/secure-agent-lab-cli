// Package deployment reads what a lab directory says about itself.
//
// The record lives at .sal/installed.json inside the deployment. It is not
// bookkeeping for this CLI's convenience: check-drift.sh in the stack repo
// looks for the same file and degrades to guessing installed providers from
// filenames without it.
package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir is the per-deployment state directory, relative to the lab root.
const Dir = ".sal"

// RecordFile is the install record inside Dir.
const RecordFile = "installed.json"

// ErrNotALab is returned when a directory is not, and is not inside, a lab.
var ErrNotALab = errors.New("not a secure-agent-lab deployment (no " + Dir + "/" + RecordFile + " here or above)")

// Record is the contents of .sal/installed.json.
type Record struct {
	// StackTag is the stack release this deployment is pinned to, in tag
	// spelling.
	StackTag string `json:"stack_tag"`

	// StackCommit is the commit that tag resolved to when the bank was
	// fetched. A tag is mutable, so the tag alone does not say what was
	// actually installed — without this, "pinned to v1.9.0" is a claim about
	// intent rather than a record of fact.
	StackCommit string `json:"stack_commit,omitempty"`

	// Installed lists every bank entry installed into this deployment.
	Installed []Entry `json:"installed"`
}

// Entry records one installed bank entry: what it was, where its files went,
// and which slot the installer assigned it.
type Entry struct {
	Name string `json:"name"`

	// Slot is the NNN prefix assigned to the proxy addon. The bank never bakes
	// one in; the installer picks the lowest free number in the manifest's
	// band, and this is where that decision is written down.
	Slot int `json:"slot"`

	// SchemaVersion is the manifest generation this entry was installed from,
	// recorded rather than re-derived later.
	SchemaVersion int `json:"schema_version"`

	// Files are the paths written, relative to the lab root, so that an
	// uninstall removes exactly what an install added.
	Files []string `json:"files"`

	// StackTag is the release the entry's files came from, which may be older
	// than the deployment's current pin if it has not been upgraded since.
	StackTag string `json:"stack_tag,omitempty"`
}

// Find walks up from start looking for a lab root. Returns the lab root
// directory and its record.
func Find(start string) (root string, rec *Record, err error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", nil, err
	}
	for {
		path := filepath.Join(dir, Dir, RecordFile)
		if _, statErr := os.Stat(path); statErr == nil {
			rec, err := load(path)
			if err != nil {
				return "", nil, err
			}
			return dir, rec, nil
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", nil, statErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil, ErrNotALab
		}
		dir = parent
	}
}

func load(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &rec, nil
}

// UsedSlots returns the addon slots already taken in this deployment, so the
// installer can assign the lowest free one in a band.
func (r *Record) UsedSlots() map[int]string {
	used := make(map[int]string, len(r.Installed))
	for _, e := range r.Installed {
		used[e.Slot] = e.Name
	}
	return used
}
