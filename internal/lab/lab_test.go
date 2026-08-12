package lab

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// composeProjectName is what Docker Compose accepts. A name that fails this
// produces a compose file that will not load — and since the name also scopes
// Docker volumes, and volume scoping is what gives each lab its own CA and its
// own audit trail, an unusable name is a boundary problem rather than a
// cosmetic one.
var composeProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func nameFor(t *testing.T, dir string) string {
	t.Helper()
	n, err := NameFor(dir)
	if err != nil {
		t.Fatalf("NameFor(%q): %v", dir, err)
	}
	return n
}

// The property the hash suffix exists for. Two projects sharing a basename
// must not share a lab: one proxy, one audit trail and one set of injected
// credentials across two projects is exactly what per-project labs prevent.
func TestNameForDistinguishesSameBasename(t *testing.T) {
	a := nameFor(t, "/home/dev/work/api")
	b := nameFor(t, "/home/dev/personal/api")

	if a == b {
		t.Fatalf("two projects named api collided on %q", a)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "api-") {
			t.Errorf("%q should still be recognisable as the api project", n)
		}
	}
}

func TestNameForIsDeterministic(t *testing.T) {
	const dir = "/home/dev/work/api"
	if a, b := nameFor(t, dir), nameFor(t, dir); a != b {
		t.Errorf("same path gave %q then %q; a lab would be orphaned on every run", a, b)
	}
}

// A relative path must resolve before hashing, or the same project reached
// from two working directories becomes two labs.
func TestNameForResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	abs := nameFor(t, dir)

	t.Chdir(dir)
	if got := nameFor(t, "."); got != abs {
		t.Errorf("NameFor(\".\") = %q, want %q", got, abs)
	}
}

// The inverse of the collision problem, and the more dangerous half: one
// project reached by two spellings must not become two labs, because two live
// labs would both be injecting credentials for it. Real on macOS, where /tmp
// and /var are symlinks, and anywhere someone keeps projects behind a link.
func TestNameForResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-project")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if viaLink, viaReal := nameFor(t, link), nameFor(t, real); viaLink != viaReal {
		t.Errorf("same directory gave %q via a symlink and %q directly; that is two labs for one project", viaLink, viaReal)
	}
}

// A name can be derived for a directory that does not exist yet — resolution
// falls back rather than failing.
func TestNameForTolerantOfMissingPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")
	if n := nameFor(t, missing); !composeProjectName.MatchString(n) {
		t.Errorf("NameFor(%q) = %q", missing, n)
	}
}

// Project directories are named by people, and compose is stricter than the
// filesystem.
func TestNameForProducesUsableComposeNames(t *testing.T) {
	cases := map[string]string{
		"plain":            "/p/acme",
		"uppercase":        "/p/ACME",
		"dots and case":    "/p/My-API.v2",
		"spaces":           "/p/my project",
		"leading digit":    "/p/2fa-service",
		"leading dash":     "/p/-hidden",
		"leading score":    "/p/_internal",
		"only punctuation": "/p/---",
		"non-ascii":        "/p/éclair",
		"trailing dot":     "/p/service.",
		"filesystem root":  "/",
	}

	seen := map[string]string{}
	for label, dir := range cases {
		t.Run(label, func(t *testing.T) {
			got := nameFor(t, dir)
			if !composeProjectName.MatchString(got) {
				t.Fatalf("NameFor(%q) = %q, which Docker Compose will reject", dir, got)
			}
			if prev, ok := seen[got]; ok {
				t.Errorf("NameFor(%q) = %q, already produced by %q", dir, got, prev)
			}
			seen[got] = dir
		})
	}
}

func writePointer(t *testing.T, dir string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, PointerDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PointerDir, PointerFile), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindWalksUp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	project := t.TempDir()
	writePointer(t, project, `{"name": "demo-lab", "stack_tag": "v1.9.0"}`)

	deep := filepath.Join(project, "src", "pkg", "inner")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	l, p, err := Find(deep)
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "demo-lab" {
		t.Errorf("name = %q", l.Name)
	}
	if p.StackTag != "v1.9.0" {
		t.Errorf("stack tag = %q", p.StackTag)
	}
	// The project is the directory holding the pointer, not the one Find
	// started from — that is what the compose file mounts at /workspace.
	if l.ProjectDir != project {
		t.Errorf("project dir = %q, want %q", l.ProjectDir, project)
	}
	if !strings.HasSuffix(filepath.Dir(l.Dir), filepath.Join("secure-agent-lab", "labs")) {
		t.Errorf("lab dir = %q, want it under the labs root", l.Dir)
	}
}

func TestFindWithoutPointer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, _, err := Find(t.TempDir()); !errors.Is(err, ErrNoLab) {
		t.Fatalf("err = %v, want ErrNoLab", err)
	}
}

// A pointer is a checked-in file whose contents become a filesystem path, so
// it is validated rather than trusted. A name that escapes the labs directory
// would let a repo decide where sal reads a deployment from.
func TestFindRejectsUntrustworthyPointers(t *testing.T) {
	cases := map[string]string{
		"traversal":      `{"name": "../../../etc", "stack_tag": "v1.9.0"}`,
		"nested path":    `{"name": "a/b", "stack_tag": "v1.9.0"}`,
		"parent":         `{"name": "..", "stack_tag": "v1.9.0"}`,
		"absolute":       `{"name": "/etc/passwd", "stack_tag": "v1.9.0"}`,
		"empty name":     `{"stack_tag": "v1.9.0"}`,
		"malformed json": `{"name": `,
	}

	for label, contents := range cases {
		t.Run(label, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			project := t.TempDir()
			writePointer(t, project, contents)

			l, _, err := Find(project)
			if err == nil {
				t.Fatalf("Find returned lab %q for pointer %s, want refusal", l.Dir, contents)
			}
			if errors.Is(err, ErrNoLab) {
				t.Errorf("a bad pointer should not read as an absent one: %v", err)
			}
		})
	}
}

func TestWritePointerRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()

	want := Pointer{Name: "demo-lab", StackTag: "v1.9.0"}
	if err := WritePointer(project, want); err != nil {
		t.Fatal(err)
	}

	_, got, err := Find(project)
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Errorf("round trip gave %+v, want %+v", *got, want)
	}
}

// The pointer is meant to be committed, which is only safe if it says nothing
// about the machine it was written on.
func TestPointerCarriesNoMachineDetail(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()

	if err := WritePointer(project, Pointer{Name: "demo-lab", StackTag: "v1.9.0"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(project, PointerDir, PointerFile))
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.ContainsAny(s, `/\`) {
			t.Errorf("%q = %q contains a path; the pointer must be committable", k, s)
		}
	}
	if strings.Contains(string(raw), project) {
		t.Error("the pointer leaks the absolute project path")
	}

	// It is also the one file sal writes outside the config directory, and it
	// belongs to the repo rather than to sal's private state.
	info, err := os.Stat(filepath.Join(project, PointerDir, PointerFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("pointer mode = %o, want 644", perm)
	}
}

func TestExistsTracksTheComposeFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writePointer(t, project, `{"name": "demo-lab", "stack_tag": "v1.9.0"}`)

	l, _, err := Find(project)
	if err != nil {
		t.Fatal(err)
	}

	// A pointer whose deployment was never created, or was deleted, is a real
	// state that commands report rather than crash on.
	if l.Exists() {
		t.Error("Exists() is true before anything was created")
	}

	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ComposeFile(), []byte("name: demo-lab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.Exists() {
		t.Error("Exists() is false with a compose file present")
	}
}
