package cli

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/drift"
	"github.com/lpezet/secure-agent-lab-cli/internal/installer"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

func newDriftCmd() *cobra.Command {
	var showDiff bool
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Report files in this lab that differ from its pinned stack release",
		Long: "The check for the problem this CLI exists to fix.\n\n" +
			"A deployment's compose file builds every image from the stack at its pinned\n" +
			"tag, but the files that ENFORCE the boundary — proxy/*.py, broker/*.js,\n" +
			"cred-gateway/*.conf — are bind-mounted from the deployment's own directories.\n" +
			"Repinning moves the images and leaves those files exactly as they were, with\n" +
			"nothing in `docker compose ps` to show for it. This is what sees that.\n\n" +
			"It compares against the release the deployment is PINNED to, so it answers \"is\n" +
			"this lab what it claims to be?\". To ask \"what would moving to a newer release\n" +
			"change?\" instead, that is `sal upgrade --dry-run`.\n\n" +
			"Exits non-zero when anything is off, so it can be a CI step.\n\n" +
			"Note that scripts/check-drift.sh stays in the stack repo even though this\n" +
			"exists. It is dependency-free bash that works for someone who never installs\n" +
			"sal, and that is worth a little duplication. It also answers a harder question:\n" +
			"having no install record, it has to guess which example a deployment came from,\n" +
			"where this one compares against what sal wrote down.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDrift(cmd, showDiff)
		},
	}
	cmd.Flags().BoolVar(&showDiff, "show-diff", false, "print what differs, not just which files")
	return cmd
}

func runDrift(cmd *cobra.Command, showDiff bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	if !l.Exists() {
		return fmt.Errorf("lab %q has no deployment at %s; run `sal init`", l.Name, l.Dir)
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}

	// By COMMIT, not by tag. A tag can move, and a lab that reads a different
	// tree than the one it was built from would report drift that is the
	// tag's, not the deployment's.
	commit, err := commitFor(cmd, rec)
	if err != nil {
		return err
	}

	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return err
	}
	defer tree.Close()

	addons, err := fetchBaseAddons(cmd, commit)
	if err != nil {
		return err
	}
	defer addons.Close()

	// An entry the operator wrote themselves is compared against their own
	// providers directory. Comparing it against the bank would report every
	// one of its files as an entry that cannot be resolved — which is a true
	// statement about the bank and a wrong one about the deployment.
	local, err := localBank()
	if err != nil {
		return err
	}

	expected, unresolved, owned := expectedFiles(b, local, addons.Dir, rec)

	report, err := drift.Check(l.Dir, expected, owned)
	if err != nil {
		return err
	}
	for _, f := range unresolved {
		report.Add(f)
	}
	if err := compareCompose(l, rec.StackTag, report); err != nil {
		return err
	}

	fmt.Fprintf(out, "deployment  %s\n", l.Dir)
	fmt.Fprintf(out, "pinned      %s", rec.StackTag)
	if commit != "" {
		fmt.Fprintf(out, " (%s)", shortCommit(commit))
	}
	fmt.Fprintf(out, "\nproject     %s\n\n", l.ProjectDir)

	for _, f := range report.Findings {
		fmt.Fprintf(out, "%-8s %-30s %s\n", f.Kind, f.Path, f.Detail)
		if showDiff && f.Kind == drift.Drift {
			printDiff(out, f, l.Dir)
		}
	}

	fmt.Fprintf(out, "\nsummary     %d drift · %d missing · %d stale · %d unowned\n",
		report.Count(drift.Drift), report.Count(drift.Missing),
		report.Count(drift.Stale), report.Count(drift.Unowned))

	if !report.Failed() {
		fmt.Fprintf(errOut, "\nThis lab is what it says it is: every file it owns matches %s.\n"+
			"That is a known list, not a review — it cannot see a way of being unsafe\n"+
			"nobody has thought of.\n", rec.StackTag)
		return nil
	}

	explainFindings(errOut, report, rec.StackTag)

	// Non-zero, so this can be the CI step that catches a lab still running a
	// file its release replaced.
	return fmt.Errorf("this lab is not what %s ships", rec.StackTag)
}

// expectedFiles works out every file this deployment should hold at its pin.
//
// Three sources, and they are not interchangeable: the stack's own proxy
// addons, which every deployment has regardless of providers; the files each
// recorded bank entry installs; and the files an entry USED to install and no
// longer does. Returns the expected set, findings for entries that could not be
// resolved at all, and the paths those entries own — which must still count as
// accounted for, or an entry sal cannot read would have all of its files
// reported as if somebody smuggled them in.
func expectedFiles(b, local *bank.Bank, addonsDir string, rec *deployment.Record) ([]drift.Expected, []drift.Finding, []string) {
	var (
		expected   []drift.Expected
		unresolved []drift.Finding
		owned      []string
	)

	// Every addon the release ships, whether or not the record lists it: one
	// that arrived in a later release is MISSING from this deployment, not
	// invisible. 000_policy.py absent means the proxy has no barrier between
	// the lab container and the broker at all.
	shipped, err := os.ReadDir(addonsDir)
	if err == nil {
		for _, it := range shipped {
			if it.IsDir() || !strings.HasSuffix(it.Name(), ".py") {
				continue
			}
			expected = append(expected, drift.Expected{
				Path:  "proxy/" + it.Name(),
				Src:   filepath.Join(addonsDir, it.Name()),
				Owner: "stack/proxy/addons/",
			})
		}
	}
	// And any the deployment records that the release has stopped shipping.
	for _, name := range rec.BaseAddons {
		if _, err := os.Stat(filepath.Join(addonsDir, name)); err == nil {
			continue
		}
		expected = append(expected, drift.Expected{Path: "proxy/" + name, Owner: "stack/proxy/addons/"})
	}

	for _, e := range rec.Installed {
		from, where := b, "bank/"+e.Name
		if e.Source == deployment.SourceLocal {
			from, where = local, "your providers/"+e.Name
		}

		plan, err := planFrom(from, e, rec.StackTag)
		if err != nil {
			// The record claims an entry this release cannot produce — a
			// renamed entry, a removed one, a slot outside its band now. Every
			// file it owns is beyond checking, so say that once rather than
			// reporting each of them as something it is not.
			unresolved = append(unresolved, drift.Finding{
				Kind:   drift.Drift,
				Path:   filepath.Join(deployment.Dir, deployment.RecordFile),
				Detail: fmt.Sprintf("records %q, which cannot be resolved from %s: %v", e.Name, sourceWord(e), err),
			})
			owned = append(owned, e.Files...)
			continue
		}

		planned := make(map[string]bool, len(plan.Files))
		for _, f := range plan.Files {
			dst := filepath.ToSlash(f.Dst)
			planned[dst] = true
			expected = append(expected, drift.Expected{
				Path:  dst,
				Src:   f.Src,
				Owner: where + "/" + path.Dir(dst) + "/",
			})
		}
		for _, prev := range e.Files {
			if !planned[filepath.ToSlash(prev)] {
				expected = append(expected, drift.Expected{Path: filepath.ToSlash(prev), Owner: where})
			}
		}
	}

	sort.Strings(owned)
	return expected, unresolved, owned
}

// planFrom replans an installed entry against the tree it came from. A nil
// bank is a providers directory that does not exist, which reads the same as
// an entry that is no longer there.
func planFrom(b *bank.Bank, e deployment.Entry, stackTag string) (*installer.Plan, error) {
	if b == nil {
		return nil, fmt.Errorf("there is no providers directory to read it from")
	}
	return installer.BuildPlanAt(b, e.Name, e.Slot, stackTag)
}

// localBank opens the operator's own providers directory, or nil when there is
// none. Nil rather than an error: most deployments have no local entries, and
// a missing directory is not a fault.
func localBank() (*bank.Bank, error) {
	dir, err := config.ProvidersDir()
	if err != nil {
		return nil, err
	}
	b, err := bank.OpenDir(dir)
	if err != nil {
		return nil, nil
	}
	return b, nil
}

func sourceWord(e deployment.Entry) string {
	if e.Source == deployment.SourceLocal {
		return "your providers directory"
	}
	return "the bank at this release"
}

// compareCompose checks the generated compose file against what sal would
// render for this deployment now.
//
// Worth its own comparison rather than being left out as "generated": the
// loopback-only observer publish, the internal lab network, and which
// directories are mounted where all live in this file, and an edit to any of
// them is a change to the boundary that no other check would see.
func compareCompose(l *lab.Lab, stackTag string, report *drift.Report) error {
	want, err := renderComposeBytes(l, stackTag)
	if err != nil {
		return err
	}
	have, err := os.ReadFile(l.ComposeFile())
	if err != nil {
		if os.IsNotExist(err) {
			report.Add(drift.Finding{Kind: drift.Missing, Path: lab.ComposeName,
				Detail: "this deployment has no compose file"})
			return nil
		}
		return err
	}
	if string(want) == string(have) {
		report.Add(drift.Finding{Kind: drift.OK, Path: lab.ComposeName, Detail: "matches what sal renders for " + stackTag})
		return nil
	}
	report.Add(drift.Finding{Kind: drift.Drift, Path: lab.ComposeName, Ref: want,
		Detail: "differs from what sal renders for " + stackTag})
	return nil
}

// printDiff shows what differs, reference first.
func printDiff(out io.Writer, f drift.Finding, deployDir string) {
	local := filepath.Join(deployDir, filepath.FromSlash(f.Path))

	var (
		d   string
		err error
	)
	switch {
	case f.Src != "":
		d, err = drift.Diff(f.Src, local)
	case f.Ref != nil:
		// A reference that was rendered rather than fetched, i.e. compose.yaml.
		var have []byte
		if have, err = os.ReadFile(local); err == nil {
			d = drift.DiffBytes(f.Ref, have)
		}
	default:
		return
	}
	if err != nil {
		fmt.Fprintf(out, "         (cannot diff: %v)\n", err)
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		fmt.Fprintf(out, "         %s\n", line)
	}
}

// explainFindings says what to do about each kind that was found, and only
// about those. A wall of advice about things that are fine is how a report
// stops being read.
func explainFindings(errOut io.Writer, report *drift.Report, tag string) {
	fmt.Fprintln(errOut)
	if report.Count(drift.Drift) > 0 || report.Count(drift.Missing) > 0 {
		fmt.Fprintf(errOut, "DRIFT and MISSING are what `sal upgrade` rewrites. Running it against the\n"+
			"same release (%s) reinstalls every file from the release itself.\n", tag)
	}
	if report.Count(drift.Stale) > 0 {
		fmt.Fprintf(errOut, "STALE files are the dangerous half of an upgrade: a cred-gateway config left\n"+
			"behind keeps whitelisting a route its entry no longer exposes. `sal upgrade`\n"+
			"deletes them.\n")
	}
	if report.Count(drift.Unowned) > 0 {
		fmt.Fprintf(errOut, "UNOWNED files arrived some other way than `sal init` or `sal providers add`.\n"+
			"sal will not touch them, and no upstream fix reaches them — check they are\n"+
			"yours before assuming they are.\n")
	}
}
