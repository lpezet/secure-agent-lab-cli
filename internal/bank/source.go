package bank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Where the stack lives. These are the hosting provider's endpoints, not
// knowledge about any bank entry — see the note beside them in
// internal/invariants/testdata/allowed-literals.txt.
const (
	defaultOwner    = "lpezet"
	defaultRepo     = "secure-agent-lab"
	defaultAPI      = "https://api.github.com"
	defaultCodeload = "https://codeload.github.com"
)

// Source is where bank trees are fetched from.
//
// Deliberately not a git clone. Contract item 2 says sal is a client that
// fetches the bank BY STACK TAG, and a tarball over HTTPS means no git binary
// on the user's machine, no credential prompts, and no partial-clone
// behaviour to reason about.
type Source struct {
	Owner, Repo string

	// APIBase and CodeloadBase exist so tests can point at an httptest server.
	// Nothing else should set them.
	APIBase      string
	CodeloadBase string

	Client *http.Client
}

// DefaultSource points at the stack repo.
func DefaultSource() *Source {
	return &Source{
		Owner:        defaultOwner,
		Repo:         defaultRepo,
		APIBase:      defaultAPI,
		CodeloadBase: defaultCodeload,
		Client: &http.Client{
			// Generous, but finite. A hung fetch during `sal upgrade` should
			// fail rather than look like a slow upgrade forever.
			Timeout: 60 * time.Second,
		},
	}
}

// tagPattern is what may be interpolated into a URL as a ref. Anything else is
// refused before it can be sent: a caller-supplied ref goes straight into a
// request path, and this is the cheap place to keep it from carrying a "..",
// a query string or a second path segment.
var tagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// shaPattern matches a full 40-character git object ID.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveTag turns a stack tag into the commit it currently points at.
//
// This exists because a tag is mutable. Recording "pinned to v1.9.0" states an
// intention; recording the commit states what is actually installed. It also
// closes a small window: the tree is downloaded BY COMMIT afterwards, so a tag
// that moves between resolving and downloading cannot substitute a different
// tree under the same name.
func (s *Source) ResolveTag(ctx context.Context, tag string) (string, error) {
	if !tagPattern.MatchString(tag) {
		return "", fmt.Errorf("%q is not a stack release tag (expected vX.Y.Z)", tag)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", s.APIBase, s.Owner, s.Repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sal")

	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", tag, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("no stack release %s exists", tag)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return "", fmt.Errorf(
				"rate-limited while resolving %s; sal caches resolved tags, so this normally only happens on a fresh machine — try again shortly", tag)
		}
		return "", fmt.Errorf("resolving %s: refused with %s", tag, resp.Status)
	default:
		return "", fmt.Errorf("resolving %s: %s", tag, resp.Status)
	}

	var body struct {
		SHA string `json:"sha"`
	}
	// Bounded: this response is a few kilobytes and nothing about it should be
	// allowed to be enormous.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("resolving %s: %w", tag, err)
	}
	sha := strings.TrimSpace(body.SHA)
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("resolving %s: got %q, which is not a commit id", tag, sha)
	}
	return sha, nil
}

// download opens the source tarball for a commit.
func (s *Source) download(ctx context.Context, sha string) (io.ReadCloser, error) {
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("%q is not a commit id", sha)
	}

	url := fmt.Sprintf("%s/%s/%s/tar.gz/%s", s.CodeloadBase, s.Owner, s.Repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sal")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", short(sha), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: %s", short(sha), resp.Status)
	}
	return resp.Body, nil
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
