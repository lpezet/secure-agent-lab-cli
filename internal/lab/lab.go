// Package lab resolves which deployment belongs to a project.
//
// Two directories, and the split is the point:
//
//	<project>/.sal/lab.json                     a committable pointer
//	~/.config/secure-agent-lab/labs/<name>/     the deployment itself
//
// The agent works in the project and cannot see the second one at all.
package lab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/config"
)

// PointerDir is the directory a project keeps its pointer in, and PointerFile
// the pointer itself.
//
// Only ever lab.json. An installed.json must NOT appear here: that record
// belongs to the deployment, is what check-drift.sh reads, and a copy in the
// project would be a second answer to "what is installed" that nothing keeps
// true.
const (
	PointerDir  = ".sal"
	PointerFile = "lab.json"
)

// ErrNoLab is returned when a directory is not, and is not inside, a project
// with a lab.
var ErrNoLab = errors.New("no lab for this directory (no " + PointerDir + "/" + PointerFile + " here or above); run `sal init` to create one")

// Pointer is <project>/.sal/lab.json.
//
// Deliberately free of absolute paths so it can be committed: it names the lab
// and the release, and nothing about the machine it was created on.
type Pointer struct {
	Name     string `json:"name"`
	StackTag string `json:"stack_tag"`
}

// Lab is a resolved deployment.
type Lab struct {
	Name       string // directory name under labs/
	Dir        string // the deployment directory
	ProjectDir string // the project this lab belongs to
}

// composeName is what Docker Compose accepts as a project name. Compose is
// stricter than the filesystem, and since the directory name doubles as the
// compose project name it has to satisfy both.
var composeName = regexp.MustCompile(`[^a-z0-9_-]+`)

// CanonicalDir resolves a project path to the one spelling sal will use for
// it, so that identity and the /workspace mount cannot disagree.
//
// Symlinks are resolved, which matters more than it looks: on macOS /tmp is a
// link to /private/tmp and /var to /private/var, so a project reached by two
// spellings of the same directory would otherwise get two labs — the exact
// inverse of the collision the name hash prevents, and worse, because two live
// labs would be injecting credentials for one project.
//
// A path that does not exist yet cannot be resolved, and falls back to the
// absolute form rather than failing: callers that only want a name should not
// need the directory to be there.
//
// Not handled, and worth knowing: a case-insensitive filesystem still gives
// /p/acme and /p/ACME two names. Lowercasing the path would fix that and break
// Linux, where those are genuinely different directories.
func CanonicalDir(projectDir string) (string, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// NameFor derives a lab name from a project's path.
//
// The hash suffix is not decoration. Two projects called "api" in different
// places must not resolve to one lab: sharing would put both behind a single
// proxy, a single audit trail and a single set of injected credentials, with
// an agent working on one holding credentials scoped for the other — the exact
// thing one-stack-per-project exists to prevent. Deriving from the path makes
// the collision impossible rather than unlikely.
func NameFor(projectDir string) (string, error) {
	abs, err := CanonicalDir(projectDir)
	if err != nil {
		return "", err
	}

	base := strings.ToLower(filepath.Base(abs))
	base = composeName.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-_")
	if base == "" || !isAlnum(rune(base[0])) {
		// Compose requires the first character to be a letter or digit.
		base = "lab-" + base
		base = strings.TrimRight(base, "-_")
	}

	sum := sha256.Sum256([]byte(abs))
	return base + "-" + hex.EncodeToString(sum[:])[:8], nil
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// Find walks up from start looking for a project pointer, and resolves the lab
// it names.
//
// It does not require the deployment to exist: a pointer whose lab directory
// has been deleted is a real state that commands need to report clearly rather
// than crash on. Callers that need the deployment call Exists.
func Find(start string) (*Lab, *Pointer, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, nil, err
	}

	for {
		path := filepath.Join(dir, PointerDir, PointerFile)
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			var p Pointer
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", path, err)
			}
			if p.Name == "" {
				return nil, nil, fmt.Errorf("%s names no lab", path)
			}
			l, err := forName(p.Name, dir)
			if err != nil {
				return nil, nil, err
			}
			return l, &p, nil
		case errors.Is(err, fs.ErrNotExist):
		default:
			return nil, nil, err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil, ErrNoLab
		}
		dir = parent
	}
}

func forName(name, projectDir string) (*Lab, error) {
	root, err := config.LabsDir()
	if err != nil {
		return nil, err
	}
	// A name out of a checked-in file becomes a path, so it is validated
	// before being joined rather than after.
	if name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("%q is not a valid lab name", name)
	}
	return &Lab{Name: name, Dir: filepath.Join(root, name), ProjectDir: projectDir}, nil
}

// Exists reports whether the deployment directory is actually there.
func (l *Lab) Exists() bool {
	_, err := os.Stat(filepath.Join(l.Dir, "compose.yaml"))
	return err == nil
}

// ComposeFile is the path commands pass to `docker compose -f`.
func (l *Lab) ComposeFile() string { return filepath.Join(l.Dir, "compose.yaml") }

// WritePointer writes <project>/.sal/lab.json.
func WritePointer(projectDir string, p Pointer) error {
	dir := filepath.Join(projectDir, PointerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// 0644, unlike everything under the config directory: this one is meant to
	// be committed and read by whoever clones the project.
	return os.WriteFile(filepath.Join(dir, PointerFile), append(b, '\n'), 0o644)
}
