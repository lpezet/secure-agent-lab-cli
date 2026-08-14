package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The trailing-newline rule, which fails in a way nobody traces back here: a
// token read out of a file carries the newline the editor put there, and the
// broker sends `Authorization: Bearer sk-…\n`. That is an invalid header, and
// it surfaces as a 401 hours later with nothing attached to explain it.
//
// The inverse is just as real, which is why multiline is the discriminator
// rather than a blanket trim: a PEM's trailing newline is part of the file.
func TestNormalize(t *testing.T) {
	cases := []struct {
		label     string
		in        string
		multiline bool
		want      string
	}{
		{"file-read token keeps no newline", "sk-test-abc123\n", false, "sk-test-abc123"},
		{"CRLF too", "sk-test-abc123\r\n", false, "sk-test-abc123"},
		{"several", "sk-test-abc123\n\n\n", false, "sk-test-abc123"},
		{"paste artefacts either side", "  sk-test-abc123  ", false, "sk-test-abc123"},
		{"nothing to do", "sk-test-abc123", false, "sk-test-abc123"},
		{"a PEM keeps its trailing newline", "--TEST BLOB--\nabc\n--END TEST BLOB--\n", true,
			"--TEST BLOB--\nabc\n--END TEST BLOB--\n"},
		{"and its interior verbatim", "line1\n\nline2\n", true, "line1\n\nline2\n"},
	}

	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if got := string(Normalize([]byte(c.in), c.multiline)); got != c.want {
				t.Errorf("Normalize(%q, %v) = %q, want %q", c.in, c.multiline, got, c.want)
			}
		})
	}
}

// A credential is not a path, and the overwhelmingly common case is that
// ResolveFile finds nothing and the typed value is stored as typed. Anything
// else here would make the prompt ask a question on every credential.
func TestResolveFileIgnoresOrdinaryCredentials(t *testing.T) {
	t.Chdir(t.TempDir())

	for _, value := range []string{
		"sk-test-0123456789abcdef",
		"gXs-notarealprefix-example",
		"--TEST BLOB-- PRIVATE KEY",
		"",
		"   ",
		"a value with spaces",
		"/nonexistent/path/to/nothing",
		"~/also-not-there",
		strings.Repeat("x", 8192), // longer than any path
		"has\x00nul",
	} {
		if ref := ResolveFile([]byte(value)); ref != nil {
			t.Errorf("ResolveFile(%.20q) = %+v, want nil", value, ref)
		}
	}
}

// A multi-line value can never be a path, and this is what lets the prompt
// consult the FIRST line of a paste without a PEM ever being mistaken for one.
func TestResolveFileIgnoresAnythingWithANewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ref := ResolveFile([]byte(path + "\nsecond line")); ref != nil {
		t.Errorf("ResolveFile of a two-line value = %+v, want nil", ref)
	}
}

// The case the feature exists for: a path typed at a prompt that asked for a
// key. Without this, sal writes the path as though it were the credential.
func TestResolveFileFindsARealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	body := []byte("--TEST BLOB--\nabc\n--END TEST BLOB--\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	ref := ResolveFile([]byte(path))
	if ref == nil {
		t.Fatal("ResolveFile returned nil for an existing file")
	}
	if !ref.Readable() {
		t.Fatalf("Readable() is false for %+v", ref)
	}
	if ref.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", ref.Size, len(body))
	}

	got, err := ref.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("Read() = %q, want %q", got, body)
	}
}

// Surrounding whitespace comes free with a paste and must not stop the file
// being recognised.
func TestResolveFileTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(path, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}

	if ref := ResolveFile([]byte("  " + path + "  \n")); ref == nil {
		t.Error("whitespace around a path stopped it resolving")
	}
}

// A relative path is how someone who just downloaded a key refers to it.
func TestResolveFileAcceptsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	ref := ResolveFile([]byte("key.pem"))
	if ref == nil {
		t.Fatal("a relative path did not resolve")
	}
	if !filepath.IsAbs(ref.Path) {
		t.Errorf("Path = %q, want it resolved to an absolute path for the confirmation", ref.Path)
	}
}

// Each of these EXISTS but cannot be copied. The important half is that they
// come back non-nil: silently falling through to "store what was typed" would
// turn a permissions typo into a credential file containing a path.
func TestResolveFileReportsUnusableTargetsRatherThanIgnoringThem(t *testing.T) {
	dir := t.TempDir()

	sub := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "toobig")
	if err := os.WriteFile(big, make([]byte, MaxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}

	for label, path := range map[string]string{"directory": sub, "oversized": big} {
		t.Run(label, func(t *testing.T) {
			ref := ResolveFile([]byte(path))
			if ref == nil {
				t.Fatal("returned nil, so the value would be stored as typed")
			}
			if ref.Readable() {
				t.Errorf("Readable() is true for %+v", ref)
			}
			if _, err := ref.Read(); err == nil {
				t.Error("Read() succeeded on something Readable() rejected")
			}
			if !strings.Contains(ref.Describe(), filepath.Base(path)) {
				t.Errorf("Describe() = %q, which does not name the file", ref.Describe())
			}
		})
	}
}

// Copying a key out of a world-readable file leaves the world-readable file
// exactly where it was. sal writes 0600, so staying quiet about the source
// would imply a tightening that did not happen.
func TestLooseSourceDetection(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		mode os.FileMode
		want bool
	}{
		"world readable": {0o644, true},
		"group readable": {0o640, true},
		"owner only":     {0o600, false},
		"read only":      {0o400, false},
	}

	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(label, " ", "-"))
			if err := os.WriteFile(path, []byte("v"), c.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, c.mode); err != nil {
				t.Fatal(err)
			}
			if got := ResolveFile([]byte(path)).LooseSource(); got != c.want {
				t.Errorf("LooseSource() = %v, want %v for %o", got, c.want, c.mode)
			}
		})
	}
}

// Describe is printed to the terminal, so it is the one place a value could
// leak into scrollback.
func TestDescribeNeverPrintsContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	const value = "sk-test-supersecretvalue"
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := ResolveFile([]byte(path)).Describe(); strings.Contains(got, value) {
		t.Errorf("Describe() = %q, which contains the file's contents", got)
	}
}

func TestStoreWritesTightAndStatsWithoutReading(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets")}

	if st := s.Stat("absent.token"); st.Set {
		t.Error("Stat reported an absent credential as set")
	}

	if err := s.Write("a.token", []byte("value")); err != nil {
		t.Fatal(err)
	}
	// The directory is created by the write, and the broker mounts it.
	info, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != DirPerm {
		t.Errorf("directory mode = %o, want %o", perm, DirPerm)
	}

	st := s.Stat("a.token")
	if !st.Set {
		t.Fatal("Stat did not see the credential just written")
	}
	if st.Mode.Perm() != Perm {
		t.Errorf("credential mode = %o, want %o", st.Mode.Perm(), Perm)
	}
	if st.Loose() {
		t.Error("a freshly written credential reads as loose")
	}
}

// Overwriting an existing credential must not inherit its mode, which is what
// os.WriteFile alone would do.
func TestStoreWriteTightensAnExistingFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	path := s.Path("a.token")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Write("a.token", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if st := s.Stat("a.token"); st.Mode.Perm() != Perm {
		t.Errorf("mode after overwrite = %o, want %o", st.Mode.Perm(), Perm)
	}
}

func TestStoreFilesToleratesAnAbsentDirectory(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "never-created")}
	files, err := s.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("Files() = %v, want none", files)
	}
}
