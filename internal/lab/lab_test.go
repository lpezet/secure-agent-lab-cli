package lab

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lpezet/secure-agent-lab-cli/internal/schema"
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
	writePointer(t, project, `{"schema_version": 1, "name": "demo-lab", "stack_tag": "v1.9.0"}`)

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
		"traversal":      `{"schema_version": 1, "name": "../../../etc", "stack_tag": "v1.9.0"}`,
		"nested path":    `{"schema_version": 1, "name": "a/b", "stack_tag": "v1.9.0"}`,
		"parent":         `{"schema_version": 1, "name": "..", "stack_tag": "v1.9.0"}`,
		"absolute":       `{"schema_version": 1, "name": "/etc/passwd", "stack_tag": "v1.9.0"}`,
		"empty name":     `{"schema_version": 1, "stack_tag": "v1.9.0"}`,
		"malformed json": `{"schema_version": 1, "name": `,
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

// A pointer written before the format carried a generation is refused rather
// than assumed to be the oldest one.
func TestFindRefusesAnUnversionedPointer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writePointer(t, project, `{"name": "demo-lab", "stack_tag": "v1.9.0"}`)

	_, _, err := Find(project)
	if err == nil {
		t.Fatal("want a refusal for a pointer with no schema_version")
	}
	if errors.Is(err, ErrNoLab) {
		t.Errorf("an unversioned pointer is not an absent one: %v", err)
	}
}

func TestWritePointerRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()

	want := Pointer{Name: "demo-lab", StackTag: "v1.9.0"}
	if err := WritePointer(project, want); err != nil {
		t.Fatal(err)
	}
	// WritePointer stamps the generation it writes, so that is what comes back.
	want.SchemaVersion = schema.Current

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
	writePointer(t, project, `{"schema_version": 1, "name": "demo-lab", "stack_tag": "v1.9.0"}`)

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

// All is the inventory `sal labs list` reports over. Every deviation it can
// meet is here rather than filtered out, because an inventory that hides the
// odd ones reports a smaller machine than the operator has — and a lab that
// does not look like a deployment can still have containers up.
func TestAllReturnsEveryDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	labs := filepath.Join(root, "secure-agent-lab", "labs")
	for _, name := range []string{"zulu-00000000", "alpha-11111111", "half-created"} {
		if err := os.MkdirAll(filepath.Join(labs, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file is not a lab, and a compose file is not required to be one:
	// "half-created" has no compose.yaml and is still reported.
	if err := os.WriteFile(filepath.Join(labs, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range got {
		names = append(names, l.Name)
		if filepath.Dir(l.Dir) != labs {
			t.Errorf("%s is at %q, want it under the labs root", l.Name, l.Dir)
		}
		// Set only by Find, which learns it by walking up from a directory
		// that holds the pointer. From this side the project is a claim in the
		// deployment's record, and the difference is what `sal labs list`
		// checks.
		if l.ProjectDir != "" {
			t.Errorf("%s carries a project dir %q that All cannot know", l.Name, l.ProjectDir)
		}
	}
	want := "alpha-11111111,half-created,zulu-00000000"
	if strings.Join(names, ",") != want {
		t.Errorf("All() = %v, want %s in that order", names, want)
	}
}

// A machine with no labs directory yet is not an error: `sal labs list` on a
// fresh install must say "no labs", not fail.
func TestAllOnAFreshMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := All()
	if err != nil {
		t.Fatalf("All() on a machine with no labs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("All() = %v, want nothing", got)
	}
}

// PointerAt answers a different question from Find, and the difference is the
// point of it existing: "does THIS directory point at a lab" rather than
// "which lab does this directory work under". An ancestor's pointer is the
// right answer to the second and a wrong answer to the first — it would report
// a lab nothing points at as healthy.
func TestPointerAtDoesNotWalkUp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	project := t.TempDir()
	writePointer(t, project, `{"schema_version": 1, "name": "demo-lab", "stack_tag": "v1.9.0"}`)

	p, err := PointerAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo-lab" {
		t.Errorf("name = %q", p.Name)
	}

	inner := filepath.Join(project, "src")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// Reported as fs.ErrNotExist specifically, so a caller can tell a missing
	// pointer from a malformed one.
	if _, err := PointerAt(inner); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PointerAt(%q) = %v, want fs.ErrNotExist", inner, err)
	}
}

// The exact shape of generation 1 of the pointer, as COMPATIBILITY.md
// publishes it.
//
// This file is COMMITTED into a user's git history, so it is read by whatever
// sal they have next month and by everyone they work with. A field added here
// travels into repositories and cannot be taken back, which is why the field
// set is asserted rather than left to whoever edits the struct next.
func TestThePointerFormatIsWhatIsPublished(t *testing.T) {
	dir := t.TempDir()
	if err := WritePointer(dir, Pointer{Name: "api-3f2a1b0c", StackTag: "v1.9.0"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, PointerDir, PointerFile))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"schema_version", "name", "stack_tag"} {
		if _, present := raw[want]; !present {
			t.Errorf("the pointer no longer carries %q", want)
		}
	}
	if len(raw) != 3 {
		t.Errorf("the pointer has %d fields, want exactly 3: %v\n"+
			"A field added here is committed into other people's repositories. "+
			"If this is deliberate, update COMPATIBILITY.md.", len(raw), raw)
	}
}

// THE property of this file, and the reason it is the pointer rather than the
// record that gets committed: it must describe no machine. A path, a username
// or a home directory in here is one that travels to a colleague's checkout
// and is wrong there.
func TestThePointerDescribesNoMachine(t *testing.T) {
	dir := t.TempDir()
	if err := WritePointer(dir, Pointer{Name: "api-3f2a1b0c", StackTag: "v1.9.0"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dir, PointerDir, PointerFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/", "\\", os.Getenv("HOME")} {
		if forbidden == "" {
			continue
		}
		if strings.Contains(string(body), forbidden) {
			t.Errorf("the pointer contains %q, which describes this machine:\n%s", forbidden, body)
		}
	}
}
