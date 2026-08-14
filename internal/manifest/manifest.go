// Package manifest reads a bank entry's provider.json.
//
// The contract it mirrors lives in the stack repo at
// bank/schema/provider.schema.json. Nothing in this package may know the name
// of any particular bank entry: the manifest is data, this is a generic reader
// over it, and internal/invariants holds a test that fails if that ever stops
// being true.
//
// There is deliberately no JSON-schema library here. The schema is
// additionalProperties:false with a closed field set, so
// json.Decoder.DisallowUnknownFields is exactly that check, and it reports the
// offending field by name instead of by JSON pointer.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/stackver"
)

// Filename is the manifest's name inside a bank entry directory.
const Filename = "provider.json"

// SupportedSchemaVersions is the fixed set of manifest generations this build
// understands.
//
// The rule is written into the schema field's own description and is binding
// on any installer: support a fixed set and REFUSE anything higher. A manifest
// from the future may declare a control this code does not know to apply, and
// an install that silently omits a control is worse than one that refuses to
// run. So this is not a floor and not a "best effort".
//
// It is a set rather than an equality check against the schema's `const` so
// that understanding two generations at once is a data change here, not a
// rewrite of the comparison.
var SupportedSchemaVersions = []int{1}

// Band is the load_band enum: which numeric band the proxy addon installs into.
type Band string

const (
	BandPolicy   Band = "policy"
	BandProvider Band = "provider"
	BandPost     Band = "post"
)

// SlotRange returns the inclusive [lo, hi] addon-number range for the band.
//
// The three band NAMES above are machine-readable from the schema's load_band
// enum, but these NUMBERS are not — they appear only in that field's
// description prose. This is therefore the one constant this CLI has to
// restate, and so the one place it can silently desync from the stack. If a
// band boundary ever moves over there, this function is what has to move with
// it, and nothing will fail here to tell you.
func (b Band) SlotRange() (lo, hi int, ok bool) {
	switch b {
	case BandPolicy:
		return 0, 9, true
	case BandProvider:
		return 10, 899, true
	case BandPost:
		return 900, 999, true
	}
	return 0, 0, false
}

// Manifest is a decoded provider.json.
//
// Required-and-scalar fields are pointers so that "absent" is distinguishable
// from "present and zero" — the difference between a manifest that predates a
// field and one that sets it to 0 or false is exactly the kind of thing this
// file must not paper over.
type Manifest struct {
	SchemaVersion *int              `json:"schema_version"`
	Name          string            `json:"name"`
	Summary       string            `json:"summary"`
	MinStack      string            `json:"min_stack"`
	LoadBand      Band              `json:"load_band"`
	Hosts         []string          `json:"hosts"`
	Secrets       []Secret          `json:"secrets,omitempty"`
	Config        []Config          `json:"config,omitempty"`
	BrokerRoutes  []Route           `json:"broker_routes"`
	LabEnv        map[string]string `json:"lab_env,omitempty"`
	LabSetup      string            `json:"lab_setup,omitempty"`
}

// Secret is a value that lives in a file under the secrets dir: 0600, prompted
// with no echo, never in argv. Distinct from Config in storage, permissions and
// prompt — never conflate the two.
type Secret struct {
	Env       string `json:"env"`
	File      string `json:"file"`
	Prompt    string `json:"prompt"`
	Multiline bool   `json:"multiline,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
}

// Config is a non-secret value that lives in .env, passed by value.
type Config struct {
	Env     string `json:"env"`
	Prompt  string `json:"prompt"`
	Help    string `json:"help,omitempty"`
	Default string `json:"default,omitempty"`
}

// Route is one route the broker provider registers.
//
// Exposed is a pointer because it is required and false is the load-bearing
// value: a route that forgot to say so must not read as "not whitelisted, fine".
type Route struct {
	Path    string `json:"path"`
	Exposed *bool  `json:"exposed"`
}

// IsExposed reports whether cred-gateway whitelists this route. Absent reads as
// false, but Validate rejects absent before anything can call this.
func (r Route) IsExposed() bool { return r.Exposed != nil && *r.Exposed }

var (
	namePat     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	minStackPat = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	envPat      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// Load reads and validates the manifest of the bank entry rooted at dir.
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, Filename)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Decode parses a manifest and validates its shape. It does NOT apply the
// schema_version or min_stack checks — those are separate calls because they
// carry different severities and different failure messages, and the order
// they run in is load-bearing. See CheckSchemaVersion and CheckMinStack.
func Decode(r io.Reader) (*Manifest, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields() // the schema's additionalProperties:false

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("malformed manifest: %w", err)
	}
	// A second JSON value in the file is not a manifest with an oddity, it is
	// a file whose contents nobody agrees on.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("malformed manifest: trailing content after the JSON object")
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest against the parts of the schema that constrain
// shape: required fields, patterns, enums, minimum lengths.
func (m *Manifest) Validate() error {
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if m.SchemaVersion == nil {
		bad("schema_version is required")
	}
	if m.Name == "" {
		bad("name is required")
	} else if !namePat.MatchString(m.Name) {
		bad("name %q does not match %s", m.Name, namePat)
	}
	if m.Summary == "" {
		bad("summary is required")
	}
	if m.MinStack == "" {
		bad("min_stack is required")
	} else if !minStackPat.MatchString(m.MinStack) {
		bad("min_stack %q does not match %s", m.MinStack, minStackPat)
	}
	if m.LoadBand == "" {
		bad("load_band is required")
	} else if _, _, ok := m.LoadBand.SlotRange(); !ok {
		bad("load_band %q is not one of %s, %s, %s", m.LoadBand, BandPolicy, BandProvider, BandPost)
	}
	if len(m.Hosts) == 0 {
		bad("hosts is required and must list at least one hostname")
	}
	for i, h := range m.Hosts {
		if strings.TrimSpace(h) == "" {
			bad("hosts[%d] is empty", i)
		}
	}

	if len(m.BrokerRoutes) == 0 {
		// Listing only the exposed routes would make this documentation;
		// listing all of them is what makes it a control.
		bad("broker_routes is required and must list every route the provider registers")
	}
	for i, r := range m.BrokerRoutes {
		if !strings.HasPrefix(r.Path, "/") {
			bad("broker_routes[%d].path %q must begin with /", i, r.Path)
		}
		if r.Exposed == nil {
			bad("broker_routes[%d] (%s) must state exposed", i, r.Path)
		}
	}

	// Two declarations must never write to one name. The schema cannot say so
	// — uniqueness across array items is not something a closed field set
	// expresses — so it is checked here.
	//
	// This is not tidiness. `file` is how an operator NAMES a credential
	// (`sal secrets set anthropic anthropic-auth.token`), so a duplicate would
	// make one of them unaddressable, and whichever was prompted for second
	// would silently overwrite the first. A duplicate `env` is the same bug in
	// the deployment's .env, where the second value wins and the first
	// provider route reads a credential meant for another.
	seenFile := map[string]int{}
	seenEnv := map[string]int{}
	dup := func(seen map[string]int, key, what string, i int) {
		key = strings.ToLower(key)
		if key == "" {
			return
		}
		if first, ok := seen[key]; ok {
			bad("%s is declared twice, at index %d and %d; two declarations cannot share one %s", what, first, i, what)
			return
		}
		seen[key] = i
	}

	for i, s := range m.Secrets {
		if !envPat.MatchString(s.Env) {
			bad("secrets[%d].env %q does not match %s", i, s.Env, envPat)
		}
		if s.File == "" {
			bad("secrets[%d].file is required", i)
		}
		if s.Prompt == "" {
			bad("secrets[%d].prompt is required", i)
		}
		// Lowercased, so a manifest that would be unambiguous on Linux and
		// collide on a case-insensitive filesystem is caught in both places.
		dup(seenFile, s.File, "secrets[].file "+strconv.Quote(s.File), i)
		dup(seenEnv, s.Env, "env "+strconv.Quote(s.Env), i)
	}
	for i, c := range m.Config {
		if !envPat.MatchString(c.Env) {
			bad("config[%d].env %q does not match %s", i, c.Env, envPat)
		}
		if c.Prompt == "" {
			bad("config[%d].prompt is required", i)
		}
		// Shares the secrets' env namespace on purpose: both end up in the
		// same .env, so a config that reuses a secret's name would overwrite
		// the path to a credential with a literal.
		dup(seenEnv, c.Env, "env "+strconv.Quote(c.Env), i)
	}
	for k := range m.LabEnv {
		if !envPat.MatchString(k) {
			bad("lab_env key %q does not match %s", k, envPat)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("invalid manifest:\n  - %s", strings.Join(problems, "\n  - "))
}

// CheckSchemaVersion is the FIRST check, before anything else in the file is
// read as meaningful. A generation above the supported set is refused, not
// warned about: this code cannot know what it would be skipping.
func (m *Manifest) CheckSchemaVersion() error {
	if m.SchemaVersion == nil {
		return errors.New("manifest does not declare schema_version, so there is no way to tell whether this build understands it")
	}
	for _, ok := range SupportedSchemaVersions {
		if *m.SchemaVersion == ok {
			return nil
		}
	}
	return fmt.Errorf(
		"manifest declares schema_version %d, which this build does not understand (it supports %s); "+
			"it may describe a control that would be silently skipped, so this is refused rather than attempted — upgrade sal",
		*m.SchemaVersion, joinInts(SupportedSchemaVersions))
}

// CheckMinStack is the SECOND check, and it must run before any file is
// written. Installing an entry into a deployment pinned below its min_stack
// fails at runtime, not at install — the classic shape being MODULE_NOT_FOUND
// on require("../audit") long after whoever ran the install has walked away.
//
// stackTag may be given in either spelling; see internal/stackver.
func (m *Manifest) CheckMinStack(stackTag string) error {
	need, err := stackver.Parse(m.MinStack)
	if err != nil {
		return fmt.Errorf("manifest min_stack: %w", err)
	}
	have, err := stackver.Parse(stackTag)
	if err != nil {
		return fmt.Errorf("deployment stack version: %w", err)
	}
	if have.Less(need) {
		return fmt.Errorf(
			"entry %q needs stack %s or later, but this deployment is pinned to %s; "+
				"installing anyway would succeed now and fail inside the container later",
			m.Name, need.Tag(), have.Tag())
	}
	return nil
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x)
	}
	return strings.Join(parts, ", ")
}
