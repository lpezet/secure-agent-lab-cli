package secrets

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store is the directory the broker mounts.
//
// Machine-wide, not per-lab, and that is the fact every operation here has to
// keep in view: one credential file is read by every lab on this machine that
// installed the provider declaring it. Storing a credential is therefore not a
// local act, and overwriting one is a rotation for all of them.
type Store struct{ Dir string }

// State is what is known about one credential file WITHOUT reading it.
//
// There is no field for the value and no field derived from it — not a length,
// not a prefix, not a fingerprint. `sal secrets list` is a control, and a
// control that leaks a little of what it is guarding is a worse one than a
// control that says only "set".
type State struct {
	File     string
	Path     string
	Set      bool
	Mode     os.FileMode
	Modified time.Time
	Size     int64
}

// Loose reports a credential readable by users other than its owner.
func (s State) Loose() bool { return s.Set && s.Mode.Perm()&0o077 != 0 }

// Path returns where a named credential file lives.
func (s Store) Path(file string) string { return filepath.Join(s.Dir, file) }

// Stat describes one credential file without opening it.
func (s Store) Stat(file string) State {
	st := State{File: file, Path: s.Path(file)}
	info, err := os.Stat(st.Path)
	if err != nil {
		return st
	}
	st.Set = true
	st.Mode = info.Mode()
	st.Modified = info.ModTime()
	st.Size = info.Size()
	return st
}

// Write stores a credential, creating the directory if needed.
//
// The mode is set explicitly after the write because os.WriteFile does not
// chmod a file that already existed — a credential that arrived at 0644 by
// some other route must not keep that mode just because sal was not the thing
// that created it.
func (s Store) Write(file string, value []byte) error {
	if err := os.MkdirAll(s.Dir, DirPerm); err != nil {
		return err
	}
	path := s.Path(file)
	if err := os.WriteFile(path, value, Perm); err != nil {
		return err
	}
	_, err := EnsureMode(path)
	return err
}

// Files lists what is actually in the directory, sorted.
//
// Used by `sal secrets list` to find credentials no installed provider claims.
// An unclaimed file is not clutter: it is a live credential mounted into every
// broker on this machine that nothing references, which is worth reporting for
// the same reason a forgotten lab is.
func (s Store) Files() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	return files, nil
}
