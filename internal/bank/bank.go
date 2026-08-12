// Package bank fetches and caches bank trees from the stack repo.
//
// The bank is data. This package knows how to obtain it and how to enumerate
// what is in it; it knows nothing about any particular entry, and must not
// learn. An entry is "a directory containing provider.json" — which is what
// makes `sal providers add <anything>` work by someone dropping a directory
// into the stack repo, with no change here.
package bank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
)

// subtree is the directory inside the stack repo that this package extracts.
// Everything else in the archive is discarded: sal installs bank entries, and
// has no business holding a copy of the stack's compose files or test suite.
const subtree = "bank"

// completeMarker records provenance and, by existing, says the directory beside
// it was fully extracted. It is written before the atomic rename into place, so
// a cache directory can never be half a bank.
const completeMarker = ".sal-cache.json"

// entryNamePattern mirrors the schema's `name`. Applied before any name from a
// caller is joined onto a path.
var entryNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Provenance is what the cache records about a fetched tree.
type Provenance struct {
	Tag       string    `json:"tag"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetched_at"`
	Files     int       `json:"files"`
}

// Bank is one fetched bank tree, on disk.
type Bank struct {
	dir  string
	prov Provenance
}

// Options controls how Open obtains a tree.
type Options struct {
	// CacheDir defaults to the bank cache under the config directory.
	CacheDir string

	// Source defaults to the stack repo.
	Source *Source

	// Refresh re-resolves the tag even when it is already cached, which is how
	// a moved tag gets noticed. The tree itself is still reused if the commit
	// is unchanged.
	Refresh bool

	// Offline refuses to touch the network. A cached tag succeeds; anything
	// else fails saying so, rather than hanging on a timeout.
	Offline bool

	Limits Limits
}

// Open returns the bank tree for a stack tag, fetching it if it is not cached.
func Open(ctx context.Context, tag string, opts Options) (*Bank, error) {
	cache, err := newCache(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	src := opts.Source
	if src == nil {
		src = DefaultSource()
	}
	lim := opts.Limits
	if lim.MaxEntries == 0 {
		lim = DefaultLimits()
	}

	// The cached tag → commit mapping is what makes the common case free. It
	// is a cache of a mutable pointer, which is exactly why --refresh exists.
	sha, known := cache.lookup(tag)

	if !known || opts.Refresh {
		if opts.Offline {
			if !known {
				return nil, fmt.Errorf("stack %s is not cached and --offline was given", tag)
			}
		} else {
			resolved, err := src.ResolveTag(ctx, tag)
			if err != nil {
				// A tag already resolved once is better than nothing when the
				// network is the problem rather than the tag.
				if !known {
					return nil, err
				}
			} else {
				sha = resolved
			}
		}
	}

	// Guards every path below that joins the commit onto a directory or a URL.
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("could not determine which commit stack %s points at", tag)
	}

	dir := cache.commitDir(sha)
	if prov, err := readProvenance(dir); err == nil {
		if err := cache.record(tag, sha); err != nil {
			return nil, err
		}
		prov.Tag = tag
		return &Bank{dir: dir, prov: prov}, nil
	}

	if opts.Offline {
		return nil, fmt.Errorf("stack %s (%s) is not in the cache and --offline was given", tag, short(sha))
	}

	prov, err := fetch(ctx, src, cache, tag, sha, lim)
	if err != nil {
		return nil, err
	}
	if err := cache.record(tag, sha); err != nil {
		return nil, err
	}
	return &Bank{dir: cache.commitDir(sha), prov: *prov}, nil
}

// OpenDir wraps an already-extracted bank tree, for tests and for a local
// checkout. It performs no network access and records no provenance.
func OpenDir(dir string) (*Bank, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return &Bank{dir: dir}, nil
}

func fetch(ctx context.Context, src *Source, c *cache, tag, sha string, lim Limits) (*Provenance, error) {
	body, err := src.download(ctx, sha)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	// Extract into a scratch directory and rename into place, so a failure
	// part-way leaves no directory that looks complete.
	tmp, err := os.MkdirTemp(c.root, ".partial-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	files, err := extractSubtree(body, subtree, tmp, lim)
	if err != nil {
		return nil, fmt.Errorf("extracting stack %s: %w", tag, err)
	}

	prov := Provenance{Tag: tag, Commit: sha, FetchedAt: time.Now().UTC(), Files: files}
	if err := writeJSON(filepath.Join(tmp, completeMarker), prov); err != nil {
		return nil, err
	}

	dest := c.commitDir(sha)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		// Another sal fetched the same commit first, which is fine — the
		// content is identical by construction.
		if readErr := func() error { _, e := readProvenance(dest); return e }(); readErr == nil {
			return &prov, nil
		}
		return nil, err
	}
	return &prov, nil
}

// Dir is the extracted bank directory.
func (b *Bank) Dir() string { return b.dir }

// Tag is the stack release this tree was fetched for.
func (b *Bank) Tag() string { return b.prov.Tag }

// Commit is what that tag resolved to. Record this, not just the tag.
func (b *Bank) Commit() string { return b.prov.Commit }

// List returns the entry names in the bank, sorted.
//
// An entry is a directory containing a manifest. That rule is the whole reason
// a new provider needs no code here — and it is also what keeps schema/ and
// README.md from being mistaken for entries, without naming either.
func (b *Bank) List() ([]string, error) {
	items, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, it := range items {
		if !it.IsDir() || !entryNamePattern.MatchString(it.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(b.dir, it.Name(), manifest.Filename)); err != nil {
			continue
		}
		names = append(names, it.Name())
	}
	sort.Strings(names)
	return names, nil
}

// EntryDir returns the directory for a named entry.
func (b *Bank) EntryDir(name string) (string, error) {
	if !entryNamePattern.MatchString(name) {
		return "", fmt.Errorf("%q is not a valid entry name", name)
	}
	dir := filepath.Join(b.dir, name)
	if _, err := os.Stat(filepath.Join(dir, manifest.Filename)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("the bank has no entry named %q at stack %s", name, b.prov.Tag)
		}
		return "", err
	}
	return dir, nil
}

// Manifest loads and validates one entry's manifest. It does not apply the
// schema_version or min_stack checks — those are the caller's, because their
// severity depends on what the caller is about to do.
func (b *Bank) Manifest(name string) (*manifest.Manifest, error) {
	dir, err := b.EntryDir(name)
	if err != nil {
		return nil, err
	}
	return manifest.Load(dir)
}

func readProvenance(dir string) (Provenance, error) {
	var p Provenance
	b, err := os.ReadFile(filepath.Join(dir, completeMarker))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	return p, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
