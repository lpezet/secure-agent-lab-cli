// Package bank obtains subtrees of the stack repo and reads bank entries out
// of them.
//
// There is deliberately no cache. A deployment already records the commit it
// is pinned to, so every command against an existing lab knows exactly which
// tree it needs and can fetch it — 209KB, about half a second — into a
// temporary directory it then throws away. A cache would buy that half second
// back in exchange for commit-keyed directories, a tag→commit index, staleness
// rules and an --offline flag, all of which are state that can be wrong. If
// repeated fetches ever become a real cost, the measurement will say so and a
// cache can go behind this same interface.
//
// The bank is data. This package knows how to obtain it and how to enumerate
// what is in it; it knows nothing about any particular entry, and must not
// learn. An entry is "a directory containing provider.json" — which is what
// makes `sal providers add <anything>` work by someone dropping a directory
// into the stack repo, with no change here.
package bank

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
)

// Subtrees of the stack repo that sal fetches. Everything else in an archive
// is discarded: sal has no business holding a copy of the stack's test suite.
const (
	// BankSubtree holds the provider entries.
	BankSubtree = "bank"

	// TemplateSubtree holds the deployment's wiring: the service graph, both
	// networks, the volumes and the mounts. Fetched at the pinned tag and used
	// VERBATIM — it is parameterised entirely by .env and lab.env, and it
	// names no provider, so there is nothing for sal to render into it.
	//
	// It also names its own tag in every build: line, which is what makes
	// `sal upgrade` a re-fetch rather than a rewrite. sal carried a copy of
	// this file until stack 1.12.0 made adopting it possible; see CLAUDE.md.
	TemplateSubtree = "template/deployment"

	// AddonsSubtree holds the proxy addons every deployment needs regardless
	// of which providers it installs — including the policy addon that stops
	// the proxy forwarding to the broker. A deployment without it has a
	// cred-gateway whitelist that can be walked around, because the proxy is
	// on both networks and will forward anywhere it is asked to.
	AddonsSubtree = "stack/proxy/addons"
)

// entryNamePattern mirrors the schema's `name`. Applied before any name from a
// caller is joined onto a path.
var entryNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Options controls how a tree is obtained.
type Options struct {
	// StackDir is a local checkout of the stack repo to read instead of
	// downloading. Serves an air-gapped machine, an unreleased branch, and the
	// test suite — all cases the deleted --offline flag served worse, because
	// this one names where the content came from rather than depending on
	// whatever a hidden cache happened to hold.
	StackDir string

	// Source defaults to the stack repo.
	Source *Source

	Limits Limits
}

// Tree is a subtree of the stack repo, on disk and ready to read.
type Tree struct {
	Dir string

	// tmp is non-empty when this tree was extracted into a temporary
	// directory that Close must remove.
	tmp string
}

// Close releases the tree. Safe to call on a tree backed by a local checkout,
// where it does nothing — the caller's directory is not sal's to delete.
func (t *Tree) Close() error {
	if t == nil || t.tmp == "" {
		return nil
	}
	return os.RemoveAll(t.tmp)
}

// FetchTree obtains one subtree of the stack repo at a commit.
//
// By commit, not by tag, because a tag is a mutable pointer and every caller
// with an existing deployment already knows the commit — it is in the install
// record. Only the commands that CHOOSE a version resolve a tag, and they do
// it once, explicitly.
func FetchTree(ctx context.Context, commit, subtree string, opts Options) (*Tree, error) {
	if opts.StackDir != "" {
		dir := filepath.Join(opts.StackDir, filepath.FromSlash(subtree))
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("%s has no %s directory: %w", opts.StackDir, subtree, err)
		}
		return &Tree{Dir: dir}, nil
	}

	src := opts.Source
	if src == nil {
		src = DefaultSource()
	}
	lim := opts.Limits
	if lim.MaxEntries == 0 {
		lim = DefaultLimits()
	}

	body, err := src.download(ctx, commit)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	tmp, err := os.MkdirTemp("", "sal-stack-")
	if err != nil {
		return nil, err
	}
	if _, err := extractSubtree(body, subtree, tmp, lim); err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("extracting %s at %s: %w", subtree, short(commit), err)
	}
	return &Tree{Dir: tmp, tmp: tmp}, nil
}

// Bank is a bank subtree on disk.
type Bank struct {
	dir string
}

// OpenDir wraps an already-extracted bank subtree.
func OpenDir(dir string) (*Bank, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	return &Bank{dir: dir}, nil
}

// Open fetches the bank subtree at a commit and wraps it. The caller closes
// the returned tree.
func Open(ctx context.Context, commit string, opts Options) (*Bank, *Tree, error) {
	tree, err := FetchTree(ctx, commit, BankSubtree, opts)
	if err != nil {
		return nil, nil, err
	}
	return &Bank{dir: tree.Dir}, tree, nil
}

// Dir is the bank directory.
func (b *Bank) Dir() string { return b.dir }

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
			return "", fmt.Errorf("the bank has no entry named %q", name)
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
