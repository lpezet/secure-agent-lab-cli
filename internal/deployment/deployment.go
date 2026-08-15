// Package deployment reads and writes a deployment's install record.
//
// The record lives at <deployment>/.sal/installed.json. That path is not
// sal's to choose: scripts/check-drift.sh in the stack repo reads
// "$DEPLOY/.sal/installed.json" and, finding nothing, degrades quietly to
// guessing installed entries from filenames. The nesting looks redundant
// inside a directory sal owns, and the reason it stays is that it makes the
// deployment an ORDINARY one — identical in shape to a hand-rolled deployment
// from examples/, so a tool that has never heard of sal still works on it.
//
// Finding which deployment belongs to a project is internal/lab's job.
package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lpezet/secure-agent-lab-cli/internal/schema"
)

// Dir is the record's directory inside a deployment, RecordFile the record.
const (
	Dir        = ".sal"
	RecordFile = "installed.json"
)

// ErrNoRecord means the directory holds no install record.
var ErrNoRecord = errors.New("deployment has no " + Dir + "/" + RecordFile)

// Record is the contents of .sal/installed.json.
type Record struct {
	// SchemaVersion is the generation of this file's format. Required, and
	// refused when above what this build supports — see internal/schema. This
	// file is read by check-drift.sh in the stack repo, so its shape is a
	// contract with another repository, not just with a later sal.
	//
	// Adding an OPTIONAL field here is not a generation event, which is the
	// opposite of the rule for bank manifests — and the difference is one line
	// of code, not a judgement call. A manifest is decoded with
	// DisallowUnknownFields (the schema says additionalProperties:false), so a
	// new field makes every older sal fail to read the file at all. This one is
	// decoded plainly, so an older sal ignores what it does not know and still
	// gets a correct answer to every question it asks. A field that CHANGES the
	// meaning of an existing one, or that a reader must honour to stay correct,
	// is a generation event wherever it lives.
	SchemaVersion int `json:"schema_version"`

	// StackTag is the release this deployment is pinned to, in tag spelling.
	StackTag string `json:"stack_tag"`

	// StackCommit is what that tag resolved to when the deployment was
	// rendered. A tag is mutable, so the tag alone records an intention;
	// this records what was actually used.
	StackCommit string `json:"stack_commit,omitempty"`

	// ProjectDir is the project this deployment serves, and the only place the
	// lab→project direction is written down. The project→lab direction is the
	// pointer at <project>/.sal/lab.json; the lab name derives from the
	// project's path but a hash cannot be inverted, so without this an
	// inventory of the labs directory cannot say what any of them is FOR.
	//
	// Absolute, and that is fine here in a way it is not in the pointer: the
	// pointer is committed and must describe no machine, while this file never
	// leaves the deployment. It is a claim rather than an observation — the
	// directory may since have been deleted, or have stopped pointing back —
	// and `sal labs list` checks it rather than trusting it.
	ProjectDir string `json:"project_dir,omitempty"`

	// Installed lists every bank entry installed here.
	Installed []Entry `json:"installed"`

	// BaseAddons are the proxy addons that come from the stack itself rather
	// than from the bank, and that every deployment needs regardless of which
	// providers it has. Recorded separately because they are not bank entries:
	// check-drift.sh reads `installed` to decide which entries a deployment
	// claims, and listing a stack addon there would have it look for a bank
	// entry that does not exist.
	BaseAddons []string `json:"base_addons,omitempty"`
}

// Where an entry came from. Empty is the bank, so a record written before this
// field existed still reads correctly.
const (
	SourceBank  = ""
	SourceLocal = "local"
)

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

	// Files are the paths written, relative to the deployment, so an uninstall
	// removes exactly what the install added.
	Files []string `json:"files"`

	// StackTag is the release the entry's files came from, which may be older
	// than the deployment's pin if it has not been upgraded since.
	StackTag string `json:"stack_tag,omitempty"`

	// Source says where the entry came from. Empty means the bank, which is
	// what every entry written before this field existed came from; "local"
	// means the operator's own providers directory.
	//
	// Recorded because the two cannot be told apart afterwards and they are
	// not equivalent: a bank entry was reviewed by whoever maintains the bank
	// and a local one was not, and `sal drift` compares each against a
	// different tree. An optional field like this one is not a generation
	// event — see SchemaVersion above.
	Source string `json:"source,omitempty"`

	InstalledAt time.Time `json:"installed_at,omitempty"`
}

// Path returns the record's path within a deployment directory.
func Path(deployDir string) string {
	return filepath.Join(deployDir, Dir, RecordFile)
}

// Load reads the record for a deployment directory.
func Load(deployDir string) (*Record, error) {
	b, err := os.ReadFile(Path(deployDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(deployDir), err)
	}
	if err := schema.Check("install record", rec.SchemaVersion, Path(deployDir)); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Save writes the record, creating .sal/ if needed.
func Save(deployDir string, rec *Record) error {
	rec.SchemaVersion = schema.Current
	if err := os.MkdirAll(filepath.Join(deployDir, Dir), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(deployDir), append(b, '\n'), 0o600)
}

// UsedSlots returns the addon slots already taken, so the installer can assign
// the lowest free one in a band.
func (r *Record) UsedSlots() map[int]string {
	used := make(map[int]string, len(r.Installed))
	for _, e := range r.Installed {
		used[e.Slot] = e.Name
	}
	return used
}

// Names returns the installed entry names, sorted.
func (r *Record) Names() []string {
	names := make([]string, 0, len(r.Installed))
	for _, e := range r.Installed {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}
