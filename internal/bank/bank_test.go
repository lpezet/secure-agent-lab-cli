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
// bank, so these tests exercise the whole path against content the rest of the
// suite also uses.
type fakeStack struct {
	t *testing.T

	mu        sync.Mutex
	resolves  int
	downloads int

	server *httptest.Server
}

func newFakeStack(t *testing.T) *fakeStack {
	t.Helper()
	f := &fakeStack{t: t}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.resolves++
		f.mu.Unlock()

		if !strings.HasSuffix(r.URL.Path, "/v1.9.0") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": shaA})
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

func (f *fakeStack) options() Options {
	return Options{Source: &Source{
		Owner:        "lpezet",
		Repo:         "secure-agent-lab",
		APIBase:      f.server.URL,
		CodeloadBase: f.server.URL,
		Client:       f.server.Client(),
	}}
}

func (f *fakeStack) counts() (resolves, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.downloads
}

// fixtureTarball packs tests/fixtures/bank into the shape a source tarball
// takes: everything under one wrapper directory named for the ref.
func fixtureTarball(t *testing.T, ref string) []byte {
	t.Helper()
	root := filepath.Join("..", "..", "tests", "fixtures", "local-stack", "bank")

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
	// A file outside every subtree, so each test proves it is dropped.
	writeFile(wrapper+"/README.md", "stack readme")
	writeFile(wrapper+"/"+BankSubtree+"/README.md", "bank readme")
	// A second subtree, so a test can prove the two do not extract over each
	// other.
	writeFile(wrapper+"/"+AddonsSubtree+"/000_policy.py", "policy addon")
	writeFile(wrapper+"/"+AddonsSubtree+"/001_allowlist.py", "allowlist addon")

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
		name := wrapper + "/" + BankSubtree + "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			writeHeader(name+"/", tar.TypeDir, 0)
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		writeFile(name, string(body))
		return nil
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

func TestOpenFetchesAndEnumerates(t *testing.T) {
	f := newFakeStack(t)

	b, tree, err := Open(context.Background(), shaA, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	names, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	if want := "[acme widget]"; fmt.Sprint(names) != want {
		t.Errorf("List() = %v, want %v", names, want)
	}

	m, err := b.Manifest("acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckSchemaVersion(); err != nil {
		t.Errorf("fetched manifest should pass its checks: %v", err)
	}

	// Fetching by commit means no tag was resolved. The caller already knew
	// which tree it wanted, which is the whole reason a cache could go.
	if resolves, downloads := f.counts(); resolves != 0 || downloads != 1 {
		t.Errorf("%d resolves and %d downloads; want 0 and 1", resolves, downloads)
	}
}

// An entry is a directory containing a manifest. That rule is what lets a new
// provider need no code here, and it keeps README.md and schema/ out of the
// listing without naming either.
func TestListRecognisesEntriesStructurally(t *testing.T) {
	f := newFakeStack(t)
	b, tree, err := Open(context.Background(), shaA, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	if _, err := os.Stat(filepath.Join(b.Dir(), "README.md")); err != nil {
		t.Fatalf("fixture setup: bank/README.md should be present: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(b.Dir(), "schema"), 0o755); err != nil {
		t.Fatal(err)
	}

	names, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name == "README.md" || name == "schema" {
			t.Errorf("%q is not an entry but was listed", name)
		}
	}
}

// One commit supplies more than one subtree, and each must come back holding
// only its own content.
func TestSubtreesOfOneCommitStaySeparate(t *testing.T) {
	f := newFakeStack(t)
	ctx := context.Background()

	bankTree, err := FetchTree(ctx, shaA, BankSubtree, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer bankTree.Close()
	addons, err := FetchTree(ctx, shaA, AddonsSubtree, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer addons.Close()

	if bankTree.Dir == addons.Dir {
		t.Fatalf("both subtrees extracted to %s", bankTree.Dir)
	}
	if _, err := os.Stat(filepath.Join(bankTree.Dir, "acme", "provider.json")); err != nil {
		t.Errorf("bank subtree is missing its entries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(addons.Dir, "000_policy.py")); err != nil {
		t.Errorf("addons subtree is missing the policy addon: %v", err)
	}
	if _, err := os.Stat(filepath.Join(addons.Dir, "acme")); err == nil {
		t.Error("the bank leaked into the addons subtree")
	}
}

// Nothing is kept between commands, so Close must actually remove what was
// extracted — otherwise "no cache" only means an unmanaged one in /tmp.
func TestCloseRemovesTheExtractedTree(t *testing.T) {
	f := newFakeStack(t)

	tree, err := FetchTree(context.Background(), shaA, BankSubtree, f.options())
	if err != nil {
		t.Fatal(err)
	}
	dir := tree.Dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("%s survived Close", dir)
	}
}

// A local checkout is read in place, and Close must not delete it — that
// directory belongs to whoever passed --stack-dir.
func TestStackDirIsReadInPlaceAndNeverDeleted(t *testing.T) {
	f := newFakeStack(t)
	local := filepath.Join("..", "..", "tests", "fixtures", "local-stack")

	opts := f.options()
	opts.StackDir = local

	tree, err := FetchTree(context.Background(), "", BankSubtree, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, BankSubtree)); err != nil {
		t.Fatalf("Close deleted the caller's checkout: %v", err)
	}

	// And nothing reached the network at all.
	if resolves, downloads := f.counts(); resolves != 0 || downloads != 0 {
		t.Errorf("a local checkout made %d resolves and %d downloads", resolves, downloads)
	}
}

func TestStackDirWithoutTheSubtree(t *testing.T) {
	opts := Options{StackDir: t.TempDir()}
	if _, err := FetchTree(context.Background(), "", BankSubtree, opts); err == nil {
		t.Fatal("want an error naming the missing subtree")
	}
}

func TestResolveRejectsRefsThatAreNotReleaseTags(t *testing.T) {
	f := newFakeStack(t)
	src := f.options().Source

	// These would otherwise be interpolated straight into a request path.
	for _, bad := range []string{"main", "../../etc/passwd", "v1.9.0/../../other", "v1.9.0?foo=bar", "", "latest"} {
		if _, err := src.ResolveTag(context.Background(), bad); err == nil {
			t.Errorf("ResolveTag(%q) should be refused", bad)
		}
	}
	if resolves, _ := f.counts(); resolves != 0 {
		t.Error("a rejected ref still reached the network")
	}
}

func TestFetchRejectsRefsThatAreNotCommits(t *testing.T) {
	f := newFakeStack(t)
	// A tag must never be interpolated here: fetches are by commit precisely
	// so a moved tag cannot change what an existing lab reads.
	for _, bad := range []string{"v1.9.0", "main", "../../etc", "", shaA[:8]} {
		if _, err := FetchTree(context.Background(), bad, BankSubtree, f.options()); err == nil {
			t.Errorf("FetchTree(%q) should be refused", bad)
		}
	}
}

func TestEntryDirRejectsNamesThatEscape(t *testing.T) {
	f := newFakeStack(t)
	b, tree, err := Open(context.Background(), shaA, f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	for _, bad := range []string{"../../etc", "..", "Acme", "acme/../..", "/etc"} {
		if got, err := b.EntryDir(bad); err == nil {
			t.Errorf("EntryDir(%q) = %q, want refusal", bad, got)
		}
	}
	if _, err := b.EntryDir("nosuchentry"); err == nil {
		t.Error("a well-formed but absent name should still error")
	}
}

// A failed extraction must leave nothing behind in the system temp directory.
func TestFailedFetchLeavesNoTemporaryDirectory(t *testing.T) {
	corrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not a gzip stream"))
	}))
	defer corrupt.Close()

	before := countSalTempDirs(t)

	opts := Options{Source: &Source{
		Owner: "lpezet", Repo: "secure-agent-lab",
		APIBase: corrupt.URL, CodeloadBase: corrupt.URL, Client: corrupt.Client(),
	}}
	if _, err := FetchTree(context.Background(), shaB, BankSubtree, opts); err == nil {
		t.Fatal("want an error for a corrupt archive")
	}

	if after := countSalTempDirs(t); after != before {
		t.Errorf("temp directories went from %d to %d; a failed fetch left one behind", before, after)
	}
}

func countSalTempDirs(t *testing.T) int {
	t.Helper()
	items, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.IsDir() && strings.HasPrefix(it.Name(), "sal-stack-") {
			n++
		}
	}
	return n
}
