package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/envfile"
)

// EntryUpgrade is one installed entry, replanned against a newer release.
type EntryUpgrade struct {
	Plan     *Plan
	Previous deployment.Entry

	// Stale are files the previous version installed that the new one does
	// not. They are DELETED, and that matters more than it sounds: a
	// cred-gateway config left behind keeps whitelisting a route the entry no
	// longer exposes, which is a widened boundary that nothing would report.
	Stale []string
}

// UpgradePlan is everything an upgrade will do, decided before it does any.
type UpgradePlan struct {
	FromTag, ToTag       string
	FromCommit, ToCommit string

	Entries []EntryUpgrade

	// addonsDir holds the new release's proxy addons.
	addonsDir string

	// BaseAddons are the addon filenames the new release ships.
	BaseAddons []string

	// StaleAddons are addons the old release shipped and the new one does not.
	StaleAddons []string
}

// Changed reports whether this upgrade would alter anything.
func (u *UpgradePlan) Changed() bool {
	return u.FromCommit != u.ToCommit || u.FromTag != u.ToTag
}

// BuildUpgradePlan replans every installed entry against a newer release.
//
// It checks EVERY entry before reporting, and refuses if any one of them
// cannot make the move — rather than upgrading the ones that can. A deployment
// running half on one release and half on another is a boundary nobody can
// describe, and "some of your providers moved" is not a state to leave someone
// in. The report names every problem at once so it can be fixed in one pass.
func BuildUpgradePlan(b *bank.Bank, addonsDir string, rec *deployment.Record, toTag, toCommit string) (*UpgradePlan, error) {
	u := &UpgradePlan{
		FromTag:    rec.StackTag,
		FromCommit: rec.StackCommit,
		ToTag:      toTag,
		ToCommit:   toCommit,
		addonsDir:  addonsDir,
	}

	var problems []string
	for _, prev := range rec.Installed {
		// Judged against the release being MOVED TO: that is what the lab will
		// be running once this completes.
		plan, err := BuildPlanAt(b, prev.Name, prev.Slot, toTag)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", prev.Name, err))
			continue
		}
		u.Entries = append(u.Entries, EntryUpgrade{
			Plan:     plan,
			Previous: prev,
			Stale:    staleFiles(prev.Files, plan.Files),
		})
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf(
			"refusing to upgrade to %s; %d installed provider(s) cannot make the move:\n  - %s\n\n"+
				"Nothing has been changed. Resolve each one — `sal providers remove <name>` for any you no longer need — and run this again",
			toTag, len(problems), strings.Join(problems, "\n  - "))
	}

	addons, err := listAddons(addonsDir)
	if err != nil {
		return nil, err
	}
	u.BaseAddons = addons
	u.StaleAddons = missingFrom(rec.BaseAddons, addons)

	return u, nil
}

// staleFiles returns the previous file set minus the new one.
func staleFiles(previous []string, now []File) []string {
	keep := make(map[string]bool, len(now))
	for _, f := range now {
		keep[f.Dst] = true
	}
	var stale []string
	for _, p := range previous {
		if !keep[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	return stale
}

func missingFrom(previous, now []string) []string {
	keep := make(map[string]bool, len(now))
	for _, n := range now {
		keep[n] = true
	}
	var gone []string
	for _, p := range previous {
		if !keep[p] {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)
	return gone
}

func listAddons(dir string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, it := range items {
		if !it.IsDir() && strings.HasSuffix(it.Name(), ".py") {
			names = append(names, it.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the stack's addon directory at %s is empty", dir)
	}
	sort.Strings(names)
	return names, nil
}

// NewConfig returns the config keys this upgrade needs a value for: declared
// by some entry's manifest and not already present in the deployment's .env.
//
// Only the new ones. An upgrade re-prompting for every value an operator has
// already set would be a good way to have them paste the wrong one.
func (u *UpgradePlan) NewConfig(deployDir string) (map[string][]string, error) {
	existing, err := envfile.Read(filepath.Join(deployDir, ".env"))
	if err != nil {
		return nil, err
	}
	needed := map[string][]string{}
	for _, e := range u.Entries {
		for _, c := range e.Plan.Manifest.Config {
			if _, ok := existing[c.Env]; !ok {
				needed[e.Plan.Manifest.Name] = append(needed[e.Plan.Manifest.Name], c.Env)
			}
		}
	}
	return needed, nil
}

// Apply performs the upgrade and returns the new record.
func (u *UpgradePlan) Apply(deployDir, secretsDir string, values map[string]Values) (*deployment.Record, error) {
	rec := &deployment.Record{
		StackTag:    u.ToTag,
		StackCommit: u.ToCommit,
		BaseAddons:  u.BaseAddons,
	}

	for _, e := range u.Entries {
		entry, err := e.Plan.Apply(deployDir, secretsDir, u.ToTag, values[e.Plan.Manifest.Name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Plan.Manifest.Name, err)
		}
		rec.Installed = append(rec.Installed, *entry)

		// Delete before moving on, so a file the new release dropped cannot
		// outlive the entry that installed it.
		for _, stale := range e.Stale {
			if err := os.Remove(filepath.Join(deployDir, stale)); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}

	proxyOut := filepath.Join(deployDir, proxyDir)
	for _, name := range u.BaseAddons {
		body, err := os.ReadFile(filepath.Join(u.addonsDir, name))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(proxyOut, name), body, 0o644); err != nil {
			return nil, err
		}
	}
	for _, name := range u.StaleAddons {
		if err := os.Remove(filepath.Join(proxyOut, name)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	return rec, nil
}
