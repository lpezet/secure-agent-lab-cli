package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The scenarios below are install.sh's, deliberately. tests/install/run.sh
// produces each of these refusals against the shell; these produce them against
// the Go. Two implementations of one policy is the cost of having an update
// path at all, and the only thing that makes it acceptable is that both are
// held to the same list — so a refusal added there belongs here too.

const tag = "v9.9.9"

// release is a fake GitHub, serving both API endpoints and the release assets.
type release struct {
	tagLatest  string // what /releases/latest answers; empty means 404
	tagList    string // what /releases?per_page=1 answers; empty means 404
	assets     map[string][]byte
	skipAssets map[string]bool // assets that 404 even though named
}

func (r *release) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	answer := func(w http.ResponseWriter, tag, shape string) {
		if tag == "" {
			http.NotFound(w, nil2req())
			return
		}
		if shape == "list" {
			// One line, both fields — the shape that made the shell's greedy
			// regex capture "prerelease" instead of the tag.
			fmt.Fprintf(w, `[{"tag_name": %q, "prerelease": true}]`, tag)
			return
		}
		fmt.Fprintf(w, `{"tag_name": %q, "prerelease": false}`, tag)
	}

	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, r.tagLatest, "one")
	})
	mux.HandleFunc("/repos/"+Repo+"/releases", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, r.tagList, "list")
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, req *http.Request) {
		name := filepath.Base(req.URL.Path)
		if r.skipAssets[name] {
			http.NotFound(w, req)
			return
		}
		body, ok := r.assets[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// nil2req exists only because http.NotFound wants a request it never reads.
func nil2req() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) }

func (r *release) config(t *testing.T) *Config {
	srv := r.server(t)
	return &Config{
		APIBase:     srv.URL,
		ReleaseBase: srv.URL + "/dl",
		GOOS:        "linux",
		GOARCH:      "amd64",
	}
}

func archiveName() string { return "sal_9.9.9_linux_amd64.tar.gz" }

// tarGz builds a release archive holding one entry.
func tarGz(t *testing.T, name string, typ byte, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: name, Typeflag: typ, Mode: 0o755, Size: int64(len(body))}
	if typ == tar.TypeSymlink {
		h.Size, h.Linkname = 0, "/etc/passwd"
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Size > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sumLine(archive string, blob []byte) []byte {
	s := sha256.Sum256(blob)
	return []byte(hex.EncodeToString(s[:]) + "  " + archive + "\n")
}

// good is a release that verifies.
func good(t *testing.T) *release {
	blob := tarGz(t, "sal", tar.TypeReg, []byte("#!/bin/sh\necho new sal\n"))
	return &release{
		tagLatest: tag,
		tagList:   tag,
		assets: map[string][]byte{
			archiveName():          blob,
			"checksums.txt":        append(sumLine("sal_9.9.9_darwin_arm64.tar.gz", []byte("other")), sumLine(archiveName(), blob)...),
			"checksums.txt.bundle": []byte("{}"),
		},
	}
}

func TestFetchVerifiesAndReturnsTheBinary(t *testing.T) {
	c := good(t).config(t)
	bin, signed, err := c.Fetch(context.Background(), tag)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if signed {
		t.Errorf("no cosign configured, so the signature cannot have been checked")
	}
	if !strings.Contains(string(bin), "new sal") {
		t.Errorf("got the wrong file out of the archive: %q", bin)
	}
}

// The bug that got shipped and could only be found by publishing: every 0.x
// release here is a pre-release on purpose, and /releases/latest EXCLUDES those
// — so it 404s and the list endpoint is the one that answers.
func TestLatestFallsBackToTheListWhenEveryReleaseIsAPreRelease(t *testing.T) {
	r := good(t)
	r.tagLatest = ""
	got, err := r.config(t).Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != tag {
		t.Errorf("resolved %q, want %q", got, tag)
	}
}

// And the stable endpoint WINS when it answers, which is what stops a
// v1.1.0-rc1 sitting above v1.0.0 in the list being handed to someone who
// asked for "latest".
func TestLatestPrefersTheStableEndpoint(t *testing.T) {
	r := good(t)
	r.tagLatest = "v1.0.0"
	r.tagList = "v1.1.0-rc1"
	got, err := r.config(t).Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "v1.0.0" {
		t.Errorf("resolved %q, want the stable release v1.0.0", got)
	}
}

func TestAnExplicitTagIsNotResolved(t *testing.T) {
	r := good(t)
	r.tagLatest, r.tagList = "v0.0.1", "v0.0.1"
	got, err := r.config(t).Resolve(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "v0.2.0" {
		t.Errorf("resolved %q; an explicit tag must be passed through untouched", got)
	}
}

func TestRefusesATamperedArchive(t *testing.T) {
	r := good(t)
	r.assets[archiveName()] = tarGz(t, "sal", tar.TypeReg, []byte("#!/bin/sh\nrm -rf /\n"))

	_, _, err := r.config(t).Fetch(context.Background(), tag)
	if err == nil {
		t.Fatal("a tampered archive was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("wrong reason: %v", err)
	}
}

func TestRefusesAReleaseWithNoChecksums(t *testing.T) {
	r := good(t)
	r.skipAssets = map[string]bool{"checksums.txt": true}

	_, _, err := r.config(t).Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "refusing to install unverified") {
		t.Fatalf("want a refusal naming unverified, got %v", err)
	}
}

// An absent line is its own refusal, not something inferred from a checker's
// exit status. This is the case the shell version got wrong until it stopped
// passing --ignore-missing.
func TestRefusesAChecksumsFileWithNoLineForThisArchive(t *testing.T) {
	r := good(t)
	r.assets["checksums.txt"] = sumLine("sal_9.9.9_darwin_arm64.tar.gz", []byte("other"))

	_, _, err := r.config(t).Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "no entry for") {
		t.Fatalf("want a refusal naming the missing entry, got %v", err)
	}
}

func TestRefusesAnArchitectureThereIsNoBuildFor(t *testing.T) {
	c := good(t).config(t)
	c.GOARCH = "riscv64"

	_, _, err := c.Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "no such release asset") {
		t.Fatalf("want a refusal naming the asset, got %v", err)
	}
}

// ------------------------------------------------------------------ cosign
//
// The policy is install.sh's: check the signature when cosign is present, say
// so when it is not, and REFUSE when it is present and fails. Never bun's
// skip-when-the-server-said-nothing.

func fakeCosign(t *testing.T, exit int, record string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\n"
	if record != "" {
		script += "printf '%s\\n' \"$*\" > " + record + "\n"
	}
	script += fmt.Sprintf("exit %d\n", exit)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifiesTheSignatureWhenCosignIsPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake cosign is a /bin/sh script")
	}
	record := filepath.Join(t.TempDir(), "argv")
	c := good(t).config(t)
	c.Cosign = fakeCosign(t, 0, record)

	_, signed, err := c.Fetch(context.Background(), tag)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !signed {
		t.Error("cosign was present and succeeded, so the signature was checked")
	}

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("cosign was never run: %v", err)
	}
	// The flags are v3's, and the identity is pinned to this repo's workflow —
	// a signature by anyone else must not satisfy it.
	for _, want := range []string{"verify-blob", "--bundle", "--certificate-identity-regexp", "--certificate-oidc-issuer"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("cosign was not asked for %s: %s", want, argv)
		}
	}
}

func TestRefusesWhenTheSignatureDoesNotVerify(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake cosign is a /bin/sh script")
	}
	c := good(t).config(t)
	c.Cosign = fakeCosign(t, 1, "")

	_, _, err := c.Fetch(context.Background(), tag)
	if err == nil {
		t.Fatal("a signature that did not verify was accepted")
	}
	if !strings.Contains(err.Error(), "did not verify") || !strings.Contains(err.Error(), "older than v3") {
		t.Errorf("the message must say what failed and that an old cosign cannot read the bundle: %v", err)
	}
}

func TestRefusesAReleaseWithNoBundleWhenCosignIsPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake cosign is a /bin/sh script")
	}
	r := good(t)
	r.skipAssets = map[string]bool{"checksums.txt.bundle": true}
	c := r.config(t)
	c.Cosign = fakeCosign(t, 0, "")

	_, _, err := c.Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "no checksums.txt.bundle") {
		t.Fatalf("want a refusal naming the missing bundle, got %v", err)
	}
}

// ----------------------------------------------------------------- archive

func TestRefusesASalEntryThatIsNotARegularFile(t *testing.T) {
	r := good(t)
	blob := tarGz(t, "sal", tar.TypeSymlink, nil)
	r.assets[archiveName()] = blob
	r.assets["checksums.txt"] = sumLine(archiveName(), blob)

	_, _, err := r.config(t).Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a symlink named sal must be refused, got %v", err)
	}
}

func TestRefusesAnArchiveWithNoBinary(t *testing.T) {
	r := good(t)
	blob := tarGz(t, "README.md", tar.TypeReg, []byte("hello"))
	r.assets[archiveName()] = blob
	r.assets["checksums.txt"] = sumLine(archiveName(), blob)

	_, _, err := r.config(t).Fetch(context.Background(), tag)
	if err == nil || !strings.Contains(err.Error(), "no sal binary") {
		t.Fatalf("want a refusal naming the missing binary, got %v", err)
	}
}

// ----------------------------------------------------------------- replace

func TestReplaceIsAtomicAndExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sal")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Replace(path, []byte("new")); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil || string(body) != "new" {
		t.Fatalf("read back %q, %v", body, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode is %v, want 0755 — an update that lands unexecutable is worse than none", info.Mode().Perm())
	}

	// Nothing left beside it: the temp file is renamed, not copied.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("left %v behind; the update should leave only the binary", names)
	}
}

func TestReplaceRefusesADirectoryItCannotWriteAndNamesTheFix(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere, so there is no refusal to observe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sal")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := Replace(path, []byte("new"))
	if err == nil {
		t.Fatal("an unwritable directory was accepted")
	}
	// EACCES on its own tells nobody what to do about it.
	if !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "SAL_INSTALL_DIR") {
		t.Errorf("the refusal must name the fix: %v", err)
	}

	// And the old binary is untouched, which is the property that matters: a
	// failed update must never leave a lab with no working sal.
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "old" {
		t.Errorf("the existing binary was disturbed: %q %v", body, err)
	}
}

// Replacing a symlink would leave the real binary in place and report success.
func TestSelfResolvesASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege there")
	}
	got, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Errorf("Self returned %q, which still resolves to %q", got, resolved)
	}
}
