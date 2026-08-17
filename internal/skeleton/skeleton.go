// Package skeleton renders a bank entry from the stack's own provider
// template.
//
// It replaces internal/scaffold, which carried a COPY of the addon API — the
// mitmproxy hook signature, the broker's require("../audit"), nginx location
// syntax. That is the image's API, which this repo does not version, so a
// skeleton kept here did not move when a deployment repinned: an addon-API
// change needed a sal release, and a scaffold from an old sal could not match
// the release a lab was pinned to. It was written down as temporary and it
// was.
//
// From stack 1.12.0 the skeleton lives at template/provider/<shape>/ and is
// fetched at the pinned commit, exactly like a bank entry — so what someone
// scaffolds is what that release actually runs, and the stack's own lint
// checks it as if it were a real entry.
//
// The rename is the whole of what this does. The upstream README states the
// contract: one placeholder token, replaced in file contents and filenames
// alike, and nothing else touched. The word "provider" appears throughout
// those files and must never move — provider.json is a fixed filename,
// "load_band": "provider" is a schema enum value, and provider= is the audit
// trail's field NAME rather than its contents. An earlier draft used
// "provider" as the placeholder and a downstream tool doing the obvious
// substitution corrupted all four.
package skeleton

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Shape is which skeleton to render — the mechanism by which a credential is
// obtained and attached, which is what actually differs between providers.
//
// Only one exists upstream. There is deliberately no --template flag here: a
// flag with one legal value is a promise about a naming scheme nobody has
// designed, and when more shapes appear they arrive as data over there.
const Shape = "static-key"

// Subtree is where the skeletons live in the stack repo.
const Subtree = "template/provider"

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// File is one rendered file, relative to the entry directory.
type File struct {
	Path string
	Mode fs.FileMode
	Body string
}

// ErrExists means the entry directory is already there.
type ErrExists struct{ Dir string }

func (e *ErrExists) Error() string { return e.Dir + " already exists" }

// Placeholder reads the token to replace out of the skeleton's OWN manifest.
//
// Never hardcoded, and that is not fastidiousness: the token is the stack's to
// choose. It was `__PROVIDER__` in the proposal and shipped as `acme`, and a
// sal that had baked in either would render a broken entry the day the other
// was used — silently, since the result is still valid JSON and valid Python.
// Reading it back means a change over there is a change to data here.
func Placeholder(shapeDir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(shapeDir, "provider.json"))
	if err != nil {
		return "", fmt.Errorf("the skeleton at this release has no manifest to read its placeholder from: %w", err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("the skeleton's manifest does not parse: %w", err)
	}
	if m.Name == "" {
		return "", fmt.Errorf("the skeleton's manifest declares no name, so there is no placeholder to replace")
	}
	return m.Name, nil
}

// Render reads a skeleton directory and returns it under the given name.
func Render(shapeDir, name string) ([]File, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("%q is not a usable entry name: lowercase letters, digits and hyphens, starting with a letter", name)
	}
	placeholder, err := Placeholder(shapeDir)
	if err != nil {
		return nil, err
	}

	var files []File
	err = filepath.WalkDir(shapeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(shapeDir, path)
		if err != nil {
			return err
		}
		files = append(files, File{
			// Filenames carry the token too — broker/acme.js becomes
			// broker/<name>.js — and an installer looks the file up by the
			// manifest's name, so a body renamed without its filename
			// produces an entry that installs nothing.
			Path: substitute(filepath.ToSlash(rel), placeholder, name),
			// 0600, like everything else sal writes here: what is in this
			// directory decides which code runs behind the credential
			// boundary.
			Mode: 0o600,
			Body: substitute(string(body), placeholder, name),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("the skeleton at %s is empty", Shape)
	}

	// The manifest first, because it is the file the others must agree with.
	sort.Slice(files, func(i, j int) bool {
		if a, b := files[i].Path == "provider.json", files[j].Path == "provider.json"; a != b {
			return a
		}
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// substitute replaces the placeholder in every case form it appears in.
//
// Both forms matter and they are not interchangeable. `acme` names files,
// routes and hosts; `ACME_TOKEN_PATH` is an environment variable, and leaving
// it behind gives an entry whose broker reads a variable the manifest never
// declares — installs fine, finds no credential, and says nothing.
//
// Hyphens become underscores in the upper form for the same reason the old
// scaffold did it: `MY-THING_TOKEN_PATH` is not a legal environment variable
// name, so a hyphenated entry name would otherwise render something no shell
// can set.
// Two forms only, and Title case is deliberately NOT one of them. A third
// branch for it was written, and it broke this: Go treats `_` as a word
// character, so strings.Title("__provider__") returns the token unchanged, and
// the branch then replaced the lowercase token with a Capitalized name —
// producing `"name": "Telegraph"` and a file called `Telegraph.js` for an
// entry called telegraph. A skeleton that spells its placeholder in prose
// keeps the spelling, which is cosmetic; renaming an entry to a name it does
// not have is not.
func substitute(s, placeholder, name string) string {
	upperName := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	s = strings.ReplaceAll(s, strings.ToUpper(placeholder), upperName)
	return strings.ReplaceAll(s, placeholder, name)
}

// Write renders the skeleton into dir, refusing to touch a directory that is
// already there.
func Write(shapeDir, dir, name string) ([]File, error) {
	files, err := Render(shapeDir, name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, &ErrExists{Dir: dir}
	}

	// 0700 throughout, like every other directory sal owns.
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(f.Body), f.Mode); err != nil {
			return nil, err
		}
	}
	return files, nil
}
