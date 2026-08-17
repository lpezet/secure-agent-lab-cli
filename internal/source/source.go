// Package source is the registry of places other than the bank that sal will
// install a provider from.
//
// Adding a source and installing from it are two separate acts, deliberately.
// Adding one is the security decision — it says whose code may run behind this
// machine's credential boundary — and installing from a source already trusted
// is not. Keeping them apart is what lets `sal providers source list` answer
// the first question at all; a fully-qualified name at install time re-decides
// trust every time, in a long string people paste out of a README.
//
// That is the shape Claude's plugin marketplaces use, and the reason is the
// same. npm's `@scope/name` is the other prior art and the lesson there is
// mostly about what goes wrong when a flat namespace is retrofitted, which is
// exactly what this exists to avoid.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/schema"
)

// File is where the registry lives, beside the labs and the secrets.
const File = "sources.json"

// Generation is the on-disk format this build writes. Reading goes through
// internal/schema, which holds the supported set — the rule is the stack's
// own: support a fixed set and refuse anything above it, because a file from
// the future may carry a field this build would ignore, and a field ignored
// here is a source trusted on terms nobody checked.
const Generation = 1

// namePattern is what a source may be called. The same shape as an entry name,
// so that `entry@source` has no character that could be either.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// repoPattern is deliberately GitHub-shaped and nothing more. Fetching is
// HTTPS against codeload, the way the bank is fetched — no git dependency on
// the user's machine — and generalising the host before anything needs it
// would be inventing a URL scheme nobody has asked for.
var repoPattern = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)$`)

// Source is one place providers can be installed from.
type Source struct {
	// Name is how it is spelled at install time, after the @.
	Name string `json:"name"`

	// Owner and Repo identify the GitHub repository.
	Owner string `json:"owner"`
	Repo  string `json:"repo"`

	// Ref is the tag or branch entries are read at. Recorded rather than
	// implied, so that installing twice from one source installs the same
	// code twice — and so `sal drift` has something to compare against.
	Ref string `json:"ref"`
}

// Registry is the whole file.
type Registry struct {
	SchemaVersion int      `json:"schema_version"`
	Sources       []Source `json:"sources"`
}

// Path is where the registry lives under a config directory.
func Path(configDir string) string { return filepath.Join(configDir, File) }

// Load reads the registry. A missing file is an empty registry, not an error:
// having added no sources is the ordinary state.
func Load(configDir string) (*Registry, error) {
	body, err := os.ReadFile(Path(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{SchemaVersion: Generation}, nil
		}
		return nil, err
	}

	var r Registry
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", Path(configDir), err)
	}
	if err := schema.Check("source registry", r.SchemaVersion, Path(configDir)); err != nil {
		return nil, err
	}
	return &r, nil
}

// Save writes the registry, 0600 like everything else here.
func Save(configDir string, r *Registry) error {
	r.SchemaVersion = Generation
	sort.Slice(r.Sources, func(i, j int) bool { return r.Sources[i].Name < r.Sources[j].Name })

	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(configDir), append(body, '\n'), 0o600)
}

// Find returns the source with this name.
func (r *Registry) Find(name string) (Source, bool) {
	for _, s := range r.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}

// Add registers a source, refusing a name that is already taken.
//
// Refusing rather than replacing: re-adding a name silently repointing it at a
// different repository is how somebody ends up installing from somewhere they
// did not choose, and the whole point of a registry is that the answer to
// "whose code is this" stays the one that was given.
func (r *Registry) Add(s Source) error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("%q is not a usable source name: lowercase letters, digits and hyphens, starting with a letter", s.Name)
	}
	if existing, ok := r.Find(s.Name); ok {
		return fmt.Errorf("a source named %q already points at %s/%s at %s.\n"+
			"Remove it first if you mean to repoint it — silently changing where a name\n"+
			"resolves is how code runs from somewhere nobody chose",
			s.Name, existing.Owner, existing.Repo, existing.Ref)
	}
	r.Sources = append(r.Sources, s)
	return nil
}

// Remove drops a source, reporting whether there was one.
func (r *Registry) Remove(name string) bool {
	for i, s := range r.Sources {
		if s.Name == name {
			r.Sources = append(r.Sources[:i], r.Sources[i+1:]...)
			return true
		}
	}
	return false
}

// ParseRepo accepts `owner/repo`, or a GitHub URL of one.
func ParseRepo(s string) (owner, repo string, err error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(s), "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	for _, prefix := range acceptedPrefixes {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}

	m := repoPattern.FindStringSubmatch(trimmed)
	if m == nil {
		// The host is interpolated from acceptedPrefixes rather than written
		// into this sentence, and not for brevity: internal/invariants forbids
		// a bank entry name as a string literal, the bank has an entry called
		// `github`, and prose naming the host trips it. Exempting the prose
		// would work until somebody reworded it, so the host is named in one
		// place that is exempt and every message points at that.
		return "", "", fmt.Errorf("%q is not a repository this can read: expected owner/repo, or a URL\n"+
			"of one such as %sowner/repo.\n"+
			"Only that host is supported today — entries are fetched over HTTPS the way the\n"+
			"bank is, so sal needs no git on your machine", s, acceptedPrefixes[0])
	}
	return m[1], m[2], nil
}

// acceptedPrefixes are the ways a repository may be written. The first is also
// what error messages and help text show as the canonical form, so the host is
// named in exactly one place.
var acceptedPrefixes = []string{"https://github.com/", "http://github.com/", "github.com/", "git@github.com:"}

// DefaultName is the source name implied by a repository when nobody says
// otherwise: the repo lowercased, minus a trailing -providers/-provider/-bank.
//
// One rule, deliberately. A first draft also stripped leading `sal-`,
// `secure-agent-lab-` and `lab-`, and the test caught what that does to a repo
// called `lab-providers`: both ends match and it collapses to `providers`,
// which names nobody. A default that is occasionally clumsy is fine — `--as`
// is one flag away and the name is printed when the source is added — while a
// default that quietly produces a misleading name is not.
func DefaultName(repo string) string {
	name := strings.ToLower(repo)
	for _, drop := range []string{"-providers", "-provider", "-bank"} {
		if trimmed := strings.TrimSuffix(name, drop); trimmed != "" && trimmed != name {
			name = trimmed
			break
		}
	}
	if !namePattern.MatchString(name) {
		return strings.ToLower(repo)
	}
	return name
}

// BankSource is how this source is fetched: the same code path the stack's own
// bank goes through, pointed at a different repository and carrying whatever
// credential the operator already has.
//
// The token is attached here rather than stored: it never reaches
// sources.json, which is a list of names and repositories and has none of the
// mode discipline the secrets directory does.
func (s Source) BankSource(ctx context.Context) *bank.Source {
	src := bank.DefaultSource()
	src.Owner, src.Repo = s.Owner, s.Repo
	src.Token = Token(ctx)
	return src
}

// Qualified splits `entry@source`. A bare name has no source, and that is not
// a lookup that falls back — see cli's resolveSource for why a third-party
// entry must always be qualified.
func Qualified(arg string) (entry, sourceName string) {
	if i := strings.LastIndex(arg, "@"); i > 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

// String is how a source is reported.
func (s Source) String() string {
	return fmt.Sprintf("%s/%s at %s", s.Owner, s.Repo, s.Ref)
}
