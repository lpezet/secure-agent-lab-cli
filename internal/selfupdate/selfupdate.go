// Package selfupdate replaces sal's own binary with a newer published release.
//
// It exists so that a tool whose whole job is telling you when a DEPLOYMENT is
// behind can also tell you when IT is, without anyone pasting a URL out of a
// README. What it must never become is a softer way in than the install script:
// the update path verifies exactly what install.sh verifies, no more and no
// less, because an update path weaker than the install path would mean the
// safest sal anyone runs is the one they installed first.
//
// That rules out the shape most self-updaters take. `bun upgrade` reads the
// asset's sha256 from the same GitHub API response that gave it the download
// URL and checks the archive against it — which catches a truncated download
// and nothing else, since the digest and the artifact carry the same authority
// — and skips the check entirely when the API reports no digest. Here a
// missing checksum, a checksums file with no line for this archive, or a
// signature that does not verify are all refusals.
//
// A wrapper on PATH was considered and rejected; see issue #47. The short
// version is that a wrapper does not remove self-replacement, it relocates it
// to a component that changes less often — and on POSIX there is nothing to
// relocate, because rename(2) is atomic and a running process keeps its open
// inode. The rename dance other tools perform is a Windows problem, and sal's
// install script points Windows at WSL.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is where releases are published. Named once and interpolated everywhere
// so no other literal in this package has to contain it.
const Repo = "lpezet/secure-agent-lab-cli"

const (
	defaultAPI     = "https://api.github.com"
	defaultWeb     = "https://github.com/lpezet/secure-agent-lab-cli"
	oidcIssuer     = "https://token.actions.githubusercontent.com"
	maxArchive     = 64 << 20 // the binary is ~8MB; this is room, not a target
	requestTimeout = 60 * time.Second
)

// ErrNoNewerRelease is not a failure. It is the ordinary answer.
var ErrNoNewerRelease = errors.New("already on the newest release")

// Config is everything that varies. APIBase and ReleaseBase exist so tests can
// point at an httptest server — the same seam internal/bank.Source uses, and
// the reason no test in this repo reaches the network.
type Config struct {
	APIBase     string
	ReleaseBase string
	Client      *http.Client

	// GOOS and GOARCH name the build to fetch. Defaulted from runtime, and
	// settable so a test can ask for a platform this one is not.
	GOOS, GOARCH string

	// Cosign is the path to a cosign binary, or empty when there is none.
	// Empty is NOT an error: install.sh checks the signature when cosign is
	// present and says "signature NOT checked" when it is not, and this holds
	// the same policy. What is an error is a cosign that is present and fails.
	Cosign string
}

func (c *Config) fill() {
	if c.APIBase == "" {
		c.APIBase = defaultAPI
	}
	if c.ReleaseBase == "" {
		c.ReleaseBase = defaultWeb + "/releases/download"
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: requestTimeout}
	}
	if c.GOOS == "" {
		c.GOOS = runtime.GOOS
	}
	if c.GOARCH == "" {
		c.GOARCH = runtime.GOARCH
	}
}

// Resolve turns "latest" into a tag, and passes an explicit tag through.
//
// Two endpoints, in this order, and the order is the whole of it:
// /releases/latest answers the newest STABLE release and EXCLUDES
// pre-releases, while /releases lists every release newest-first.
//
// Both are needed, in both eras. Today every 0.x release here is published as a
// pre-release on purpose, so the first endpoint 404s and the second answers.
// After 1.0 it inverts and the first becomes the one that matters: a
// v1.1.0-rc1 sitting above v1.0.0 in the list is not what "latest" means to
// whoever typed it, and asking only the list would hand it to them.
//
// This is the bug that got shipped once and could only be found by publishing:
// with no stable release, /releases/latest answers either nothing or whichever
// old release happened to be marked stable.
func (c *Config) Resolve(ctx context.Context, want string) (string, error) {
	c.fill()
	if want != "" && want != "latest" {
		return want, nil
	}

	for _, path := range []string{
		"/repos/" + Repo + "/releases/latest",
		"/repos/" + Repo + "/releases?per_page=1",
	} {
		tag, err := c.tagFrom(ctx, c.APIBase+path)
		if err != nil {
			return "", err
		}
		if tag != "" {
			return tag, nil
		}
	}
	return "", errors.New("could not determine the latest release")
}

// tagFrom reads the first tag_name out of either endpoint's response.
//
// Decoded rather than pattern-matched: the shell version has to grep for the
// field because a list response arrives on one line and a greedy regex there
// captures `prerelease` instead of the tag. Go can just decode both shapes.
//
// An empty answer is a real answer, not a failure: /releases/latest is a 404
// for a repo whose only releases are pre-releases.
func (c *Config) tagFrom(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking which release is newest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("asking which release is newest: %s said %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var one struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &one); err == nil && one.Tag != "" {
		return one.Tag, nil
	}
	var many []struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &many); err == nil && len(many) > 0 {
		return many[0].Tag, nil
	}
	return "", nil
}

// Result says what happened, so the caller can report it without re-deriving
// any of it.
type Result struct {
	Tag string

	// SignatureChecked is false when there is no cosign on PATH. It is never
	// false because a check failed — that is a refusal, not a result.
	SignatureChecked bool

	// Path is the binary that was replaced.
	Path string
}

// Fetch downloads a release's archive, verifies it, and returns the binary.
//
// Order matters and mirrors install.sh: the checksum is verified BEFORE
// anything is extracted, so a tampered archive is never unpacked.
func (c *Config) Fetch(ctx context.Context, tag string) ([]byte, bool, error) {
	c.fill()

	archive := fmt.Sprintf("sal_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), c.GOOS, c.GOARCH)
	base := c.ReleaseBase + "/" + tag

	blob, err := c.get(ctx, base+"/"+archive)
	if err != nil {
		return nil, false, fmt.Errorf("no such release asset: %s", archive)
	}
	sums, err := c.get(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, false, fmt.Errorf("release %s publishes no checksums.txt; refusing to install unverified", tag)
	}

	// The signature is checked over checksums.txt, before that file is trusted
	// to say anything about the archive.
	signed := false
	if c.Cosign != "" {
		bundle, err := c.get(ctx, base+"/checksums.txt.bundle")
		if err != nil {
			return nil, false, fmt.Errorf("release %s publishes no checksums.txt.bundle; refusing to install unverified", tag)
		}
		if err := c.verify(ctx, sums, bundle, tag); err != nil {
			return nil, false, err
		}
		signed = true
	}

	want, err := expectedSum(sums, archive)
	if err != nil {
		return nil, false, err
	}
	got := sha256.Sum256(blob)
	if hex.EncodeToString(got[:]) != want {
		return nil, false, fmt.Errorf("checksum mismatch for %s", archive)
	}

	bin, err := extractBinary(blob)
	if err != nil {
		return nil, false, err
	}
	return bin, signed, nil
}

func (c *Config) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s said %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

// verify runs cosign over the checksums file.
//
// Shelled out to rather than linking sigstore-go, and that is the same choice
// made for `docker compose`: the CLI is the stable contract, and a security
// tool's pitch is a small auditable dependency tree. It also means this holds
// install.sh's policy by running install.sh's command.
//
// A Sigstore BUNDLE carries the signature, the certificate and the
// transparency-log entry in one file. cosign v3 removed the flags that produced
// and checked a detached .sig plus .pem, so an older cosign cannot read this
// and the message says so rather than leaving someone to guess.
func (c *Config) verify(ctx context.Context, sums, bundle []byte, tag string) error {
	dir, err := os.MkdirTemp("", "sal-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	sumsPath := filepath.Join(dir, "checksums.txt")
	bundlePath := filepath.Join(dir, "checksums.txt.bundle")
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, c.Cosign, "verify-blob", sumsPath,
		"--bundle", bundlePath,
		"--certificate-identity-regexp", defaultWeb+"/.*",
		"--certificate-oidc-issuer", oidcIssuer,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("signature over checksums.txt did not verify.\n"+
			"  If your cosign is older than v3 it cannot read this bundle format — upgrade it,\n"+
			"  or verify by hand from %s/releases/tag/%s\n  cosign said: %s",
			defaultWeb, tag, firstLine(out))
	}
	return nil
}

// expectedSum picks this archive's line out of a checksums file covering every
// platform.
//
// An absent line is an explicit refusal rather than something inferred from a
// checker's exit status — which is how the shell version had to do it before it
// stopped passing --ignore-missing.
func expectedSum(sums []byte, archive string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == archive {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", archive)
}

// extractBinary pulls exactly the `sal` entry out of the release archive.
//
// The archive's names are COMPARED and never used to build a path, so the
// traversal cases internal/bank's extractor has to defend against cannot arise
// here: this writes one file, to a path the caller chose. What it does keep is
// that extractor's first rule — only a regular file is accepted, because a
// symlink or a hardlink named `sal` is a way to make the caller write somewhere
// else, and deciding which links are safe is harder than having none.
func extractBinary(blob []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(io.LimitReader(gz, maxArchive+1))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if filepath.Base(h.Name) != "sal" || filepath.Dir(h.Name) != "." {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("the archive's sal entry is not a regular file")
		}
		bin, err := io.ReadAll(io.LimitReader(tr, maxArchive+1))
		if err != nil {
			return nil, err
		}
		if len(bin) == 0 {
			return nil, errors.New("the archive's sal entry is empty")
		}
		return bin, nil
	}
	return nil, errors.New("the archive contains no sal binary")
}

// Replace swaps the binary at path for bin.
//
// Written beside the target and renamed over it, which is atomic on POSIX and
// is why sal needs no wrapper to update itself: the running process keeps its
// open inode and carries on, and there is never a moment where the path holds a
// half-written file. Beside it rather than in TMPDIR because a rename across
// filesystems is not atomic — and /tmp on its own mount is ordinary.
//
// A path that cannot be written is a refusal that names the fix, because the
// difference between ~/.local/bin and /usr/local/bin is one the operator can
// act on and the error is otherwise just EACCES.
func Replace(path string, bin []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".sal-update-")
	if err != nil {
		return fmt.Errorf("cannot write to %s, so sal cannot replace itself there.\n"+
			"  If sal is installed somewhere root owns, re-run this with sudo, or\n"+
			"  install into a directory you own with SAL_INSTALL_DIR: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Set before the rename, so the file is never briefly in place unexecutable.
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Self is the binary this process is running from, with symlinks resolved.
//
// Resolved because replacing a symlink would leave the real binary in place and
// report success — the update would appear to work and change nothing.
func Self() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// FindCosign reports the cosign binary to verify with, or empty when there is
// none. Empty is a supported state, not a degraded one — it is what install.sh
// reports as "signature NOT checked".
func FindCosign() string {
	path, err := exec.LookPath("cosign")
	if err != nil {
		return ""
	}
	return path
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
