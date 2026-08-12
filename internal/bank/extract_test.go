package bank

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub wraps a source tarball in one directory named for the repo and ref.
// Its exact spelling is not something to depend on, so the extractor drops the
// first component positionally — and these fixtures use a name that looks
// nothing like the real one to keep that honest.
const testRoot = "wrapper-dir-9f3a2c"

type tarEntry struct {
	name string
	typ  byte
	body string
	link string
	mode int64
	size int64 // when non-zero, overrides the real body length
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		h := &tar.Header{
			Name:     e.name,
			Typeflag: typ,
			Mode:     mode,
			Size:     size,
			Linkname: e.link,
		}
		if typ == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
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

func extract(t *testing.T, entries []tarEntry, lim Limits) (dest string, files int, err error) {
	t.Helper()
	dest = t.TempDir()
	files, err = extractSubtree(bytes.NewReader(makeTarGz(t, entries)), subtree, dest, lim)
	return dest, files, err
}

func TestExtractHappyPath(t *testing.T) {
	dest, files, err := extract(t, []tarEntry{
		{name: testRoot + "/", typ: tar.TypeDir},
		{name: testRoot + "/README.md", body: "not part of the bank"},
		{name: testRoot + "/stack/compose.yaml", body: "also not"},
		{name: testRoot + "/bank/", typ: tar.TypeDir},
		{name: testRoot + "/bank/README.md", body: "bank readme"},
		{name: testRoot + "/bank/acme/", typ: tar.TypeDir},
		{name: testRoot + "/bank/acme/provider.json", body: `{"name":"acme"}`},
		{name: testRoot + "/bank/acme/proxy/acme.py", body: "addon"},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if files != 3 {
		t.Errorf("wrote %d files, want 3", files)
	}

	got, err := os.ReadFile(filepath.Join(dest, "acme", "provider.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"name":"acme"}` {
		t.Errorf("content = %q", got)
	}

	// Everything outside bank/ is discarded. sal installs bank entries and has
	// no business keeping a copy of the stack's compose files.
	for _, unwanted := range []string{"stack", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dest, unwanted)); err == nil {
			t.Errorf("%s should not have been extracted", unwanted)
		}
	}

	// Both archives carry a README.md — one at the root, one inside bank/. They
	// collapse to the same path once the subtree is stripped, so the content is
	// what proves the right one survived.
	readme, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil {
		t.Fatalf("bank/README.md should have been extracted: %v", err)
	}
	if string(readme) != "bank readme" {
		t.Errorf("README.md = %q; the root README was extracted instead of the bank's", readme)
	}
}

// Links are refused outright rather than inspected. A symlink in a source
// archive is a link that can be made to point anywhere, and deciding which
// ones are safe is a harder problem than not having any.
func TestExtractRefusesLinks(t *testing.T) {
	cases := map[string]tarEntry{
		"symlink": {name: testRoot + "/bank/acme/evil", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"hardlink": {
			name: testRoot + "/bank/acme/evil", typ: tar.TypeLink,
			link: testRoot + "/bank/acme/provider.json",
		},
		"fifo":   {name: testRoot + "/bank/acme/evil", typ: tar.TypeFifo},
		"device": {name: testRoot + "/bank/acme/evil", typ: tar.TypeChar},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := extract(t, []tarEntry{
				{name: testRoot + "/bank/acme/provider.json", body: "{}"},
				bad,
			}, DefaultLimits())
			if err == nil {
				t.Fatal("want refusal")
			}
			if !errors.Is(err, errUnsafeArchive) {
				t.Errorf("want errUnsafeArchive, got %v", err)
			}
		})
	}
}

// Traversal is handled in two layers, and this checks the outer one: a name
// that climbs out of the subtree normalizes to something that is no longer
// under bank/, so it is never a candidate for writing. safeJoin is the inner
// layer and is tested directly below.
func TestExtractIgnoresTraversalNames(t *testing.T) {
	dest, _, err := extract(t, []tarEntry{
		{name: testRoot + "/bank/acme/provider.json", body: "{}"},
		{name: testRoot + "/bank/../../escaped.txt", body: "should not land"},
		{name: testRoot + "/bank/../sibling.txt", body: "should not land"},
		{name: "../../../etc/cron.d/evil", body: "should not land"},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	parent := filepath.Dir(dest)
	for _, escaped := range []string{"escaped.txt", "sibling.txt"} {
		if _, err := os.Stat(filepath.Join(parent, escaped)); err == nil {
			t.Errorf("%s escaped the extraction directory", escaped)
		}
		if _, err := os.Stat(filepath.Join(dest, escaped)); err == nil {
			t.Errorf("%s should not have been extracted at all", escaped)
		}
	}
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	dest := t.TempDir()
	for _, rel := range []string{
		"../escape",
		"../../escape",
		"a/../../escape",
		"/etc/passwd",
		"a/\x00b",
	} {
		if got, err := safeJoin(dest, rel); err == nil {
			t.Errorf("safeJoin(%q) = %q, want refusal", rel, got)
		}
	}

	// And it still allows the ordinary case.
	if _, err := safeJoin(dest, "acme/proxy/acme.py"); err != nil {
		t.Errorf("legitimate path refused: %v", err)
	}
}

func TestExtractEnforcesLimits(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		var entries []tarEntry
		for i := range 10 {
			entries = append(entries, tarEntry{
				name: testRoot + "/bank/acme/f" + string(rune('a'+i)),
				body: "x",
			})
		}
		lim := DefaultLimits()
		lim.MaxEntries = 4
		if _, _, err := extract(t, entries, lim); err == nil || !errors.Is(err, errUnsafeArchive) {
			t.Fatalf("want errUnsafeArchive, got %v", err)
		}
	})

	t.Run("single file too large", func(t *testing.T) {
		lim := DefaultLimits()
		lim.MaxFileBytes = 8
		_, _, err := extract(t, []tarEntry{
			{name: testRoot + "/bank/acme/provider.json", body: strings.Repeat("x", 64)},
		}, lim)
		if err == nil || !errors.Is(err, errUnsafeArchive) {
			t.Fatalf("want errUnsafeArchive, got %v", err)
		}
	})

	t.Run("total too large", func(t *testing.T) {
		lim := DefaultLimits()
		lim.MaxTotalBytes = 32
		lim.MaxFileBytes = 32
		var entries []tarEntry
		for i := range 8 {
			entries = append(entries, tarEntry{
				name: testRoot + "/bank/acme/f" + string(rune('a'+i)),
				body: strings.Repeat("x", 16),
			})
		}
		if _, _, err := extract(t, entries, lim); err == nil {
			t.Fatal("want refusal once the archive expands past the cap")
		}
	})
}

// Modes come from this code, not from the archive.
func TestExtractNormalizesModes(t *testing.T) {
	dest, _, err := extract(t, []tarEntry{
		{name: testRoot + "/bank/acme/", typ: tar.TypeDir, mode: 0o777},
		{name: testRoot + "/bank/acme/provider.json", body: "{}", mode: 0o4777},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dest, "acme", "provider.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("file mode = %o, want 644", got)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Error("setuid bit survived extraction")
	}
}

func TestExtractRefusesArchiveWithoutSubtree(t *testing.T) {
	_, _, err := extract(t, []tarEntry{
		{name: testRoot + "/README.md", body: "no bank here"},
	}, DefaultLimits())
	if err == nil {
		t.Fatal("want an error when the archive has no bank/ directory")
	}
	if !strings.Contains(err.Error(), "bank") {
		t.Errorf("error should say what was missing, got: %v", err)
	}
}
