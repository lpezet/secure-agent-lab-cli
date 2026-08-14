// Package secrets holds the policy for credential files on the host.
//
// Everything here is about files under ~/.config/secure-agent-lab/secrets/ —
// what mode they carry, how a value is normalised before it is written, and
// how an operator's typed input is turned into one. Finding WHICH file a
// credential belongs in is not this package's job and never can be: that comes
// from the bank entry's manifest, which is the only thing that knows a
// provider has one credential or two and what the broker will look for.
//
// The invariant that shapes the whole package: sal never inspects a
// credential's CONTENTS to choose a DESTINATION. The destination is read out
// of the manifest, always. ResolveFile below does look at what was typed, but
// only to ask the operator what kind of input it was — the answer changes
// where the bytes come FROM, never where they go TO, and a human confirms it.
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Perm is the mode every credential file must have, and DirPerm the mode of
// the directory holding them. The broker mounts that directory; nothing else
// under the config root is mounted anywhere.
const (
	Perm    os.FileMode = 0o600
	DirPerm os.FileMode = 0o700
)

// MaxFileSize bounds what ResolveFile will offer to copy.
//
// Credentials are small — a PEM is a couple of kilobytes and a token is a
// couple of hundred bytes. The bound exists because the operator may have
// typed a path to something that is not a credential at all, and "sal copied
// my 2GB disk image into the directory the broker mounts" should not be
// reachable by a typo.
const MaxFileSize = 1 << 20

// EnsureMode tightens a credential file that is more permissive than Perm,
// reporting whether it had to.
//
// This exists because of the path that does NOT write: a credential already
// present is reused rather than re-prompted, and reuse skipped the mode
// entirely — so a file that arrived at 0644, by hand or from an older tool,
// stayed 0644 forever while every install reported success. The enclosing
// directory is 0700, so the exposure is limited, but "limited" is not the
// property the stack's own docs promise for these files.
//
// Only ever tightens. A file at 0400 is stricter than required and is left
// alone: sal is not the only thing that may have opinions about a credential,
// and loosening one silently would be a boundary change nothing reported.
func EnsureMode(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	perm := info.Mode().Perm()
	if perm&^Perm == 0 {
		return false, nil
	}
	if err := os.Chmod(path, perm&Perm); err != nil {
		return false, err
	}
	return true, nil
}

// Normalize prepares a value for writing.
//
// The distinction is load-bearing and multiline is the right discriminator for
// it, the same flag the manifest already uses to choose a prompt widget:
//
//   - A single-line credential is trimmed. A token read out of a file almost
//     always carries the trailing newline the editor put there, and writing it
//     through produces `Authorization: Bearer sk-…\n` — an invalid header that
//     surfaces as a 401 long after anyone is watching.
//
//   - A multiline credential is written verbatim. A PEM's trailing newline is
//     part of the file and some parsers reject it without one, so trimming
//     here would break exactly the case the flag marks.
func Normalize(value []byte, multiline bool) []byte {
	if multiline {
		return value
	}
	return bytes.TrimSpace(value)
}

// FileRef is what the operator's input turned out to point at.
type FileRef struct {
	Path string      // the resolved, absolute path
	Size int64       // bytes, for the confirmation
	Mode os.FileMode // the source's mode, so a loose one can be reported
	Dir  bool        // it exists but is a directory
	Err  error       // it exists but could not be read
}

// Readable reports whether this reference can actually be copied.
func (f *FileRef) Readable() bool { return f != nil && !f.Dir && f.Err == nil }

// LooseSource reports whether the file it points at is readable by users other
// than its owner. Not a refusal — the operator's own key is theirs to store how
// they like — but worth one line, because copying a credential out of a 0644
// file leaves the 0644 file exactly where it was.
func (f *FileRef) LooseSource() bool {
	return f.Readable() && f.Mode.Perm()&0o077 != 0
}

// ResolveFile reports whether typed input names an existing file, so the
// caller can ask the operator which they meant.
//
// It returns nil when the input cannot be a path at all, which is the common
// case: a real credential is high-entropy and does not name a file on this
// machine. The point is the opposite direction. Someone pasting
// ~/Downloads/key.pem into a prompt that asked for a key is an ordinary slip,
// and without this check sal writes thirty-four bytes of path into the
// credential file and reports success — the failure arrives later, as a 401
// with nothing attached to explain it.
//
// This never decides anything on its own. A non-nil result becomes a question.
func ResolveFile(value []byte) *FileRef {
	raw := strings.TrimSpace(string(value))
	switch {
	case raw == "":
		return nil
	case len(raw) > 4096: // longer than any path this could be
		return nil
	case strings.ContainsAny(raw, "\n\x00"):
		// A path holds neither. A pasted PEM holds the first, which is why
		// callers pass only the first line of a multiline read.
		return nil
	}

	path, err := expand(raw)
	if err != nil {
		return nil
	}

	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Much the most common outcome, and the one that means "this was a
		// credential, carry on".
		return nil
	}
	if err != nil {
		// It exists in some form sal could not stat — a dangling symlink, a
		// directory it may not traverse. Surfacing this as a question beats
		// silently storing the path as if it were a credential, which is what
		// a permissions typo would otherwise become.
		return &FileRef{Path: path, Err: err}
	}
	if info.IsDir() {
		return &FileRef{Path: path, Mode: info.Mode(), Dir: true}
	}
	if !info.Mode().IsRegular() {
		return &FileRef{Path: path, Mode: info.Mode(), Err: errors.New("not a regular file")}
	}
	if info.Size() > MaxFileSize {
		return &FileRef{Path: path, Size: info.Size(), Mode: info.Mode(),
			Err: fmt.Errorf("%d bytes, larger than the %d-byte limit on a credential", info.Size(), MaxFileSize)}
	}
	return &FileRef{Path: path, Size: info.Size(), Mode: info.Mode()}
}

// Read returns the referenced file's contents.
func (f *FileRef) Read() ([]byte, error) {
	if !f.Readable() {
		return nil, fmt.Errorf("%s: %w", f.Path, f.reason())
	}
	return os.ReadFile(f.Path)
}

func (f *FileRef) reason() error {
	switch {
	case f.Dir:
		return errors.New("is a directory")
	case f.Err != nil:
		return f.Err
	}
	return nil
}

// Describe is the one line shown back to the operator before they confirm.
//
// It prints the resolved PATH, never the value. That is safe in the direction
// that matters: this line only ever appears when the typed input named a file
// that exists, and a genuine credential that also names a file on this machine
// is not a case worth designing for. It doubles as the echo the no-echo prompt
// could not give — the operator typed blind, and this is where they find out
// what landed.
func (f *FileRef) Describe() string {
	switch {
	case f.Dir:
		return fmt.Sprintf("%s is a directory", f.Path)
	case f.Err != nil:
		return fmt.Sprintf("%s: %v", f.Path, f.Err)
	}
	return fmt.Sprintf("%s (%d bytes, %04o)", f.Path, f.Size, f.Mode.Perm())
}

func expand(raw string) (string, error) {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}
	return filepath.Abs(raw)
}
