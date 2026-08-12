package bank

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lpezet/secure-agent-lab-cli/internal/config"
)

// The cache is keyed by COMMIT, not by tag.
//
// A tag-keyed cache is wrong in a way that only shows up when it matters: if a
// tag is ever moved, every machine that already fetched it keeps serving the
// old tree forever, and `sal upgrade` reports success having changed nothing.
// Keying by commit makes the tree immutable and turns the tag into what it
// actually is — a pointer, cached separately and refreshable on its own.
const (
	commitsDir = "commits"
	refsFile   = "refs.json"
)

type cache struct {
	root string
}

type ref struct {
	Commit     string    `json:"commit"`
	ResolvedAt time.Time `json:"resolved_at"`
}

func newCache(dir string) (*cache, error) {
	if dir == "" {
		var err error
		dir, err = config.BankCacheDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, commitsDir), 0o700); err != nil {
		return nil, err
	}
	return &cache{root: dir}, nil
}

func (c *cache) commitDir(sha string) string {
	return filepath.Join(c.root, commitsDir, sha)
}

func (c *cache) refsPath() string {
	return filepath.Join(c.root, refsFile)
}

func (c *cache) lookup(tag string) (string, bool) {
	refs, err := c.load()
	if err != nil {
		return "", false
	}
	r, ok := refs[tag]
	if !ok || !shaPattern.MatchString(r.Commit) {
		return "", false
	}
	return r.Commit, true
}

func (c *cache) record(tag, sha string) error {
	refs, err := c.load()
	if err != nil {
		return err
	}
	if cur, ok := refs[tag]; ok && cur.Commit == sha {
		return nil
	}
	refs[tag] = ref{Commit: sha, ResolvedAt: time.Now().UTC()}
	return c.save(refs)
}

func (c *cache) load() (map[string]ref, error) {
	b, err := os.ReadFile(c.refsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]ref{}, nil
	}
	if err != nil {
		return nil, err
	}
	refs := map[string]ref{}
	if err := json.Unmarshal(b, &refs); err != nil {
		// A corrupt cache index is not worth failing a command over: it holds
		// nothing that cannot be re-resolved with one request.
		return map[string]ref{}, nil
	}
	return refs, nil
}

// save writes through a temporary file so a concurrent reader never sees a
// half-written index, and a crash mid-write leaves the previous one intact.
func (c *cache) save(refs map[string]ref) error {
	b, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.root, ".refs-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.refsPath())
}
