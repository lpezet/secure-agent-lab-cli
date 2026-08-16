// Package installer installs a bank entry into a deployment.
//
// Split into a plan and an apply, deliberately. Every check runs and every
// destination is decided before a single byte is written, so a refusal leaves
// the deployment exactly as it was. The alternative — checking as you copy —
// means a failed install is a half-installed provider, which for a credential
// path is worse than no provider at all.
//
// Nothing here knows the name of any bank entry. Everything it does is driven
// by the manifest it was handed.
package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/egress"
	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
)

// Where each kind of file in a bank entry is installed to, relative to the
// deployment. These mirror the mounts in the generated compose file.
const (
	brokerDir  = "broker"
	proxyDir   = "proxy"
	gatewayDir = "cred-gateway"
	labDir     = "lab"
)

// egressFile is the entry's own allowlist, from stack 1.13.0. Not one of the
// four above: those are copied into the deployment as files, and this one is
// MERGED into a file the operator also writes in.
const egressFile = "allowlist"

// File is one file to copy, resolved on both sides.
type File struct {
	Src  string // absolute, in the bank cache
	Dst  string // relative to the deployment
	Mode os.FileMode
}

// Plan is everything an install will do, decided before it does any of it.
type Plan struct {
	Manifest *manifest.Manifest

	// Slot is the addon number assigned from the manifest's band. The bank
	// never bakes one in.
	Slot int

	Files []File

	// SecretEnv maps a secret's env var to the in-container path of the file
	// it will be read from. The value is a path, never the credential.
	SecretEnv map[string]string

	// SecretFiles maps a secret's env var to the host path its value must be
	// written to, so the caller can prompt and write with the right modes.
	SecretFiles map[string]string

	// LabEnv is the manifest's lab_env verbatim: literals for the lab
	// container, and never a credential.
	LabEnv map[string]string

	// Egress is what the entry says it needs to reach, from its own
	// `allowlist` file (stack 1.13.0 and newer). Empty for an entry that
	// ships none, which is every entry at an older release — and the
	// deployment then behaves exactly as it did before.
	//
	// Not derived from Manifest.Hosts, which is a different list: `hosts` is
	// where the addon ATTACHES a credential, this is where the lab MAY send a
	// request. github.com belongs in the second and must never be in the
	// first. Seeding from `hosts` would also lose the methods, and a line with
	// none defaults to GET,HEAD,OPTIONS — which reads as configured and blocks
	// every POST.
	Egress egress.Entry
}

// ErrAlreadyInstalled is returned when the entry is already recorded.
var ErrAlreadyInstalled = errors.New("already installed")

// secretsMount is where the compose file mounts the host secrets directory.
const secretsMount = "/secrets"

// BuildPlan runs every check and decides every destination.
//
// The order is not negotiable and is the whole reason this function exists as
// one place rather than being spread through the command:
//
//  1. schema_version, before anything else in the manifest is treated as
//     meaningful. A generation above what this build knows may declare a
//     control that would be silently skipped.
//  2. min_stack against what the deployment RUNS. Installing below it succeeds
//     here and fails inside a container later, which is the worst shape a
//     failure can take.
//  3. The entry's own consistency — an exposed:false route whitelisted anyway,
//     a declared host the addon never matches. Neither is visible in the
//     manifest alone.
//  4. Only then, where the files go.
func BuildPlan(b *bank.Bank, name string, rec *deployment.Record, occupied map[int]string, labStackTag string) (*Plan, error) {
	for _, e := range rec.Installed {
		if e.Name == name {
			return nil, fmt.Errorf("%q is %w (slot %03d); remove it first to reinstall", name, ErrAlreadyInstalled, e.Slot)
		}
	}

	m, entryDir, err := prepare(b, name, labStackTag)
	if err != nil {
		return nil, err
	}
	slot, err := assignSlot(m, occupied)
	if err != nil {
		return nil, err
	}
	return assemble(m, entryDir, slot)
}

// BuildPlanAt builds a plan for an entry that is ALREADY installed, keeping
// the slot it was assigned.
//
// Keeping the number is the point. The slot is the addon's filename prefix and
// therefore its load order, and load order is a security property: the policy
// band runs before providers for a reason. An upgrade that renumbered an addon
// would quietly reorder the proxy's pipeline.
func BuildPlanAt(b *bank.Bank, name string, slot int, labStackTag string) (*Plan, error) {
	m, entryDir, err := prepare(b, name, labStackTag)
	if err != nil {
		return nil, err
	}
	if lo, hi, ok := m.LoadBand.SlotRange(); !ok || slot < lo || slot > hi {
		return nil, fmt.Errorf(
			"%q is installed at slot %03d but now declares the %s band (%03d-%03d); "+
				"remove and re-add it rather than having an upgrade silently renumber it",
			name, slot, m.LoadBand, lo, hi)
	}
	return assemble(m, entryDir, slot)
}

// prepare runs every check that does not depend on where the files will go.
func prepare(b *bank.Bank, name, labStackTag string) (*manifest.Manifest, string, error) {
	m, err := b.Manifest(name)
	if err != nil {
		return nil, "", err
	}
	if err := m.CheckSchemaVersion(); err != nil {
		return nil, "", err
	}
	if err := m.CheckMinStack(labStackTag); err != nil {
		return nil, "", err
	}
	entryDir, err := b.EntryDir(name)
	if err != nil {
		return nil, "", err
	}
	if err := checkEntryConsistency(entryDir, m); err != nil {
		return nil, "", err
	}
	return m, entryDir, nil
}

// assemble decides where every file goes, once the slot is known.
func assemble(m *manifest.Manifest, entryDir string, slot int) (*Plan, error) {
	p := &Plan{
		Manifest:    m,
		Slot:        slot,
		SecretEnv:   map[string]string{},
		SecretFiles: map[string]string{},
		LabEnv:      m.LabEnv,
	}

	// The four files an entry may carry. Each is optional except in the sense
	// that an entry with none of them installs nothing.
	type candidate struct {
		src  string
		dst  string
		mode os.FileMode
	}
	candidates := []candidate{
		{filepath.Join(entryDir, brokerDir, m.Name+".js"), filepath.Join(brokerDir, m.Name+".js"), 0o644},
		{filepath.Join(entryDir, proxyDir, m.Name+".py"), filepath.Join(proxyDir, fmt.Sprintf("%03d_%s.py", slot, m.Name)), 0o644},
		{filepath.Join(entryDir, gatewayDir, m.Name+".conf"), filepath.Join(gatewayDir, m.Name+".conf"), 0o644},
	}
	if m.LabSetup != "" {
		// Named per entry rather than kept as setup.sh: several providers each
		// shipping "lab/setup.sh" would otherwise overwrite one another.
		candidates = append(candidates, candidate{
			filepath.Join(entryDir, filepath.FromSlash(m.LabSetup)), filepath.Join(labDir, m.Name+".sh"), 0o755,
		})
	}

	for _, c := range candidates {
		if _, err := os.Stat(c.src); err != nil {
			continue
		}
		p.Files = append(p.Files, File{Src: c.src, Dst: c.dst, Mode: c.mode})
	}
	if len(p.Files) == 0 {
		return nil, fmt.Errorf("bank entry %q carries no installable files", m.Name)
	}

	if body, err := os.ReadFile(filepath.Join(entryDir, egressFile)); err == nil {
		p.Egress = egress.Parse(body)
	}

	for _, s := range m.Secrets {
		p.SecretEnv[s.Env] = secretsMount + "/" + s.File
		p.SecretFiles[s.Env] = s.File
	}
	return p, nil
}

// checkEntryConsistency catches the failures that are invisible in the
// manifest alone, because they live between the manifest and a file beside it.
func checkEntryConsistency(entryDir string, m *manifest.Manifest) error {
	if err := checkNoUnexposedRouteWhitelisted(entryDir, m); err != nil {
		return err
	}
	return checkHostsAppearInAddon(entryDir, m)
}

// checkNoUnexposedRouteWhitelisted is the control that matters most here.
//
// A route marked exposed:false mints something reusable. Whitelisting it in
// cred-gateway hands the lab the credential itself rather than a scoped,
// minted artefact — which is the entire distinction the broker exists to
// maintain. Every field of such a manifest is valid, so nothing but this
// comparison finds it.
func checkNoUnexposedRouteWhitelisted(entryDir string, m *manifest.Manifest) error {
	confPath := filepath.Join(entryDir, gatewayDir, m.Name+".conf")
	conf, err := os.ReadFile(confPath)
	if err != nil {
		return nil // no gateway config is a perfectly ordinary entry
	}

	var leaked []string
	for _, r := range m.BrokerRoutes {
		if r.IsExposed() {
			continue
		}
		if mentionsRoute(string(conf), r.Path) {
			leaked = append(leaked, r.Path)
		}
	}
	if len(leaked) == 0 {
		return nil
	}
	sort.Strings(leaked)
	return fmt.Errorf(
		"refusing to install %q: its manifest marks %s as exposed:false, but %s/%s.conf whitelists %s anyway.\n"+
			"An unexposed route mints a reusable credential; exposing it hands the lab the secret itself",
		m.Name, strings.Join(leaked, ", "), gatewayDir, m.Name, plural(len(leaked), "it", "them"))
}

// mentionsRoute looks for the path as an nginx location target rather than as
// a substring, so /x/token does not match a comment about /x/tokenizer.
func mentionsRoute(conf, route string) bool {
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "location") {
			continue
		}
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '{' || r == '}' || r == ';'
		}) {
			if f == route {
				return true
			}
		}
	}
	return false
}

// checkHostsAppearInAddon enforces one direction of the hosts agreement: a
// host the manifest declares but the addon never matches is a credential that
// silently never gets injected.
//
// The other direction — a host the addon matches but the manifest omits, which
// would be undeclared egress — is deliberately NOT checked here. Deciding
// which quoted string in a Python file is a hostname is a guess, and a wrong
// guess refuses a legitimate entry. The stack repo's own check-invariants.sh
// is the right place for it, with the addon's structure in view.
func checkHostsAppearInAddon(entryDir string, m *manifest.Manifest) error {
	addon := filepath.Join(entryDir, proxyDir, m.Name+".py")
	body, err := os.ReadFile(addon)
	if err != nil {
		return nil // no addon, nothing to agree with
	}

	var missing []string
	for _, h := range m.Hosts {
		if !quotedIn(string(body), h) {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"refusing to install %q: its manifest declares %s, which %s/%s.py never mentions.\n"+
			"A declared host the addon does not match is a credential that is never injected, and the failure is silent",
		m.Name, strings.Join(missing, ", "), proxyDir, m.Name)
}

// quotedIn reports whether the host appears as a quoted literal.
//
// A bare substring search is not enough, and finding that out cost a test: an
// addon's docstring that merely NAMES a host would satisfy it, so an addon
// documenting a host it does not actually match would install clean. The rule
// is agreement with the quoted hostname literals, so that is what is checked.
func quotedIn(body, host string) bool {
	return strings.Contains(body, `"`+host+`"`) || strings.Contains(body, `'`+host+`'`)
}

// assignSlot picks the lowest free number in the manifest's band.
//
// Both sources are consulted: what the record says is installed, and what is
// actually on disk. They should agree, and if a stray addon file exists that
// the record does not know about, taking its number would silently shadow it.
func assignSlot(m *manifest.Manifest, occupied map[int]string) (int, error) {
	lo, hi, ok := m.LoadBand.SlotRange()
	if !ok {
		return 0, fmt.Errorf("manifest declares unknown load_band %q", m.LoadBand)
	}
	for n := lo; n <= hi; n++ {
		if _, taken := occupied[n]; !taken {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no free slot in the %s band (%03d-%03d); %d already occupied", m.LoadBand, lo, hi, len(occupied))
}

// OccupiedSlots reports which addon numbers are taken, from the record and
// from the files actually on disk.
//
// It returns a set rather than adding to the record, and that distinction is
// not cosmetic: an earlier version appended what it found to rec.Installed,
// and the caller then saved that record — writing the stack's own proxy addons
// into installed.json as if they were bank entries named "policy" and
// "allowlist". check-drift.sh reads that field to decide which entries a
// deployment claims, so a healthy lab reported drift forever, and `upgrade`
// refused because those entries are not in any bank. A function that reads
// state must not quietly write it.
func OccupiedSlots(deployDir string, rec *deployment.Record) map[int]string {
	occupied := rec.UsedSlots()

	entries, err := os.ReadDir(filepath.Join(deployDir, proxyDir))
	if err != nil {
		return occupied
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) < 4 || !strings.HasSuffix(name, ".py") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(name[:3], "%d", &n); err != nil {
			continue
		}
		if _, ok := occupied[n]; !ok {
			occupied[n] = name
		}
	}
	return occupied
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
