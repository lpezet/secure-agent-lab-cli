package bank

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeStack serves what the stack repo's hosting serves: a tag→commit lookup
// and a source tarball per commit. The tarball is built from the real fixture
// bank, so these tests exercise the whole path — resolve, download, extract,
// cache, enumerate, decode a manifest — against content that is also what the
// rest of the suite uses.
type fakeStack struct {
	t *testing.T

	mu        sync.Mutex
	head      string // what the tag currently points at
	resolves  int
	downloads int

	server *httptest.Server
}

func newFakeStack(t *testing.T) *fakeStack {
	t.Helper()
	f := &fakeStack{t: t, head: shaA}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.resolves++
		sha := f.head
		f.mu.Unlock()

		if !strings.HasSuffix(r.URL.Path, "/v1.9.0") && !strings.HasSuffix(r.URL.Path, "/v2.0.0") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
	})
	mux.HandleFunc("/lpezet/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.downloads++
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(fixtureTarball(f.t, path.Base(r.URL.Path)))
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeStack) source() *Source {
	return &Source{
		Owner:        "lpezet",
		Repo:         "secure-agent-lab",
		APIBase:      f.server.URL,
		CodeloadBase: f.server.URL,
		Client:       f.server.Client(),
	}
}

func (f *fakeStack) counts() (resolves, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.downloads
}

func (f *fakeStack) moveTag(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = sha
}

// fixtureTarball packs tests/fixtures/bank into the shape a source tarball
// takes: everything under one wrapper directory named for the ref.
func fixtureTarball(t *testing.T, ref string) []byte {
	t.Helper()
	root := filepath.Join("..", "..", "tests", "fixtures", "bank")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	wrapper := "secure-agent-lab-" + ref
	writeHeader := func(name string, typ byte, size int64) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: typ, Mode: 0o644, Size: size,
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(name, body string) {
		writeHeader(name, tar.TypeReg, int64(len(body)))
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	writeHeader(wrapper+"/", tar.TypeDir, 0)
	// A file outside the subtree, so every test proves it is dropped.
	writeFile(wrapper+"/README.md", "stack readme")
	// And the one the real bank carries, which is inside the subtree and is
	// therefore kept — but is still not an entry.
	writeFile(wrapper+"/"+subtree+"/README.md", "bank readme")

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := wrapper + "/" + subtree + "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			writeHeader(name+"/", tar.TypeDir, 0)
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		writeHeader(name, tar.TypeReg, int64(len(body)))
		_, err = tw.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func openBank(t *testing.T, f *fakeStack, cacheDir, tag string, mutate func(*Options)) (*Bank, error) {
	t.Helper()
	opts := Options{CacheDir: cacheDir, Source: f.source()}
	if mutate != nil {
		mutate(&opts)
	}
	return Open(context.Background(), tag, opts)
}

func TestOpenFetchesAndEnumerates(t *testing.T) {
	f := newFakeStack(t)
	cache := t.TempDir()

	b, err := openBank(t, f, cache, "v1.9.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	if b.Tag() != "v1.9.0" {
		t.Errorf("Tag() = %q", b.Tag())
	}
	// The commit, not just the tag. A tag is a mutable pointer; this is what
	// was actually installed.
	if b.Commit() != shaA {
		t.Errorf("Commit() = %q, want %q", b.Commit(), shaA)
	}

	names, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme", "widget"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("List() = %v, want %v", names, want)
	}

	m, err := b.Manifest("acme")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "acme" {
		t.Errorf("manifest name = %q", m.Name)
	}
	if err := m.CheckSchemaVersion(); err != nil {
		t.Errorf("fetched manifest should pass its checks: %v", err)
	}
}

// An entry is a directory containing a manifest. That rule is what lets a new
// provider need no code here, and it is also what keeps schema/ and README.md
// out of the listing without either being named.
func TestListRecognisesEntriesStructurally(t *testing.T) {
	f := newFakeStack(t)
	b, err := openBank(t, f, t.TempDir(), "v1.9.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The tarball carries bank/README.md; it is not an entry.
	if _, err := os.Stat(filepath.Join(b.Dir(), "README.md")); err != nil {
		t.Fatalf("fixture setup: bank/README.md should be present: %v", err)
	}
	for _, name := range mustList(t, b) {
		if name == "README.md" {
			t.Error("a file was listed as an entry")
		}
	}

	// A directory without a manifest is not an entry either.
	if err := os.MkdirAll(filepath.Join(b.Dir(), "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Dir(), "schema", "provider.schema.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range mustList(t, b) {
		if name == "schema" {
			t.Error("a directory with no manifest was listed as an entry")
		}
	}
}

func mustList(t *testing.T, b *Bank) []string {
	t.Helper()
	names, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestSecondOpenUsesTheCache(t *testing.T) {
	f := newFakeStack(t)
	cache := t.TempDir()

	if _, err := openBank(t, f, cache, "v1.9.0", nil); err != nil {
		t.Fatal(err)
	}
	r1, d1 := f.counts()
	if r1 != 1 || d1 != 1 {
		t.Fatalf("first open: %d resolves, %d downloads; want 1 and 1", r1, d1)
	}

	if _, err := openBank(t, f, cache, "v1.9.0", nil); err != nil {
		t.Fatal(err)
	}
	r2, d2 := f.counts()
	if r2 != r1 || d2 != d1 {
		t.Errorf("second open hit the network: %d resolves, %d downloads", r2, d2)
	}
}

// The reason the cache is keyed by commit rather than by tag.
//
// A tag-keyed cache serves the old tree forever once a tag moves, and
// `sal upgrade` would report success having changed nothing.
func TestMovedTagIsOnlyNoticedOnRefresh(t *testing.T) {
	f := newFakeStack(t)
	cache := t.TempDir()

	if _, err := openBank(t, f, cache, "v1.9.0", nil); err != nil {
		t.Fatal(err)
	}
	f.moveTag(shaB)

	// Without --refresh the cached pointer stands, and no request is made.
	b, err := openBank(t, f, cache, "v1.9.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Commit() != shaA {
		t.Errorf("cached commit = %q, want the original %q", b.Commit(), shaA)
	}

	// With it, the pointer is re-resolved and the new tree fetched.
	b, err = openBank(t, f, cache, "v1.9.0", func(o *Options) { o.Refresh = true })
	if err != nil {
		t.Fatal(err)
	}
	if b.Commit() != shaB {
		t.Errorf("refreshed commit = %q, want %q", b.Commit(), shaB)
	}

	// Both trees remain cached, because a commit is immutable and the old one
	// may still be what some other lab is pinned to.
	for _, sha := range []string{shaA, shaB} {
		if _, err := os.Stat(filepath.Join(cache, commitsDir, sha)); err != nil {
			t.Errorf("commit %s should still be cached: %v", short(sha), err)
		}
	}
}

func TestOfflineUsesCacheAndOtherwiseSaysSo(t *testing.T) {
	f := newFakeStack(t)
	cache := t.TempDir()

	if _, err := openBank(t, f, cache, "v1.9.0", nil); err != nil {
		t.Fatal(err)
	}

	before, _ := f.counts()
	b, err := openBank(t, f, cache, "v1.9.0", func(o *Options) { o.Offline = true })
	if err != nil {
		t.Fatalf("a cached tag should work offline: %v", err)
	}
	if b.Commit() != shaA {
		t.Errorf("Commit() = %q", b.Commit())
	}
	if after, _ := f.counts(); after != before {
		t.Error("offline made a request")
	}

	// An uncached tag fails immediately and says why, rather than hanging.
	_, err = openBank(t, f, cache, "v2.0.0", func(o *Options) { o.Offline = true })
	if err == nil {
		t.Fatal("want an error for an uncached tag while offline")
	}
	if !strings.Contains(err.Error(), "offline") {
		t.Errorf("error should mention offline, got: %v", err)
	}
}

func TestResolveRejectsRefsThatAreNotReleaseTags(t *testing.T) {
	f := newFakeStack(t)
	src := f.source()

	// These would otherwise be interpolated straight into a request path.
	for _, bad := range []string{
		"main",
		"../../etc/passwd",
		"v1.9.0/../../other",
		"v1.9.0?foo=bar",
		"",
		"latest",
	} {
		if _, err := src.ResolveTag(context.Background(), bad); err == nil {
			t.Errorf("ResolveTag(%q) should be refused", bad)
		}
	}
	if before, _ := f.counts(); before != 0 {
		t.Error("a rejected ref still reached the network")
	}
}

func TestEntryDirRejectsNamesThatEscape(t *testing.T) {
	f := newFakeStack(t)
	b, err := openBank(t, f, t.TempDir(), "v1.9.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../../etc", "..", "Acme", "acme/../..", "/etc"} {
		if got, err := b.EntryDir(bad); err == nil {
			t.Errorf("EntryDir(%q) = %q, want refusal", bad, got)
		}
	}
	if _, err := b.EntryDir("nosuchentry"); err == nil {
		t.Error("a well-formed but absent name should still error")
	}
}

// A failed extraction must leave no directory that a later run mistakes for a
// complete one.
func TestInterruptedFetchLeavesNoUsableCacheEntry(t *testing.T) {
	cache := t.TempDir()
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": shaA})
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("this is not a gzip stream"))
	}))
	defer truncated.Close()

	src := &Source{
		Owner: "lpezet", Repo: "secure-agent-lab",
		APIBase: truncated.URL, CodeloadBase: truncated.URL, Client: truncated.Client(),
	}
	_, err := Open(context.Background(), "v1.9.0", Options{CacheDir: cache, Source: src})
	if err == nil {
		t.Fatal("want an error for a corrupt archive")
	}
	if _, statErr := os.Stat(filepath.Join(cache, commitsDir, shaA)); statErr == nil {
		t.Error("a failed fetch left a cache directory behind")
	}

	// And no scratch directories are left lying around either.
	items, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".partial-") {
			t.Errorf("scratch directory %s was not cleaned up", it.Name())
		}
	}
}
