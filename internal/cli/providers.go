package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/installer"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
	"github.com/lpezet/secure-agent-lab-cli/internal/prompt"
)

// newProvidersCmd builds the `sal providers` group.
//
// Everything under here is generic over the bank. `sal providers add <name>`
// must work by someone dropping bank/<name>/ into the stack repo with zero
// changes in this repo — so no command here may branch on which provider it
// was handed. internal/invariants holds the test that says so.
func newProvidersCmd() *cobra.Command {
	group := newGroup("providers", "Install and manage credential providers from the bank")
	group.AddCommand(
		newProvidersListCmd(),
		newProvidersAddCmd(),
		newProvidersCreateCmd(),
		newProvidersRemoveCmd(),
	)
	return group
}

func newProvidersListCmd() *cobra.Command {
	var (
		available bool
		stack     string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List providers installed in this lab",
		Long: "By default, what is installed here — read from .sal/installed.json rather than\n" +
			"guessed from filenames.\n\n" +
			"With --available, what the bank offers at the stack release this lab is pinned\n" +
			"to. Not at the newest release: offering entries from a bank newer than the lab\n" +
			"runs would mean offering ones whose min_stack it does not satisfy, and that\n" +
			"failure lands at runtime inside a container rather than here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if available {
				return runProvidersAvailable(cmd, stack)
			}
			return runProvidersInstalled(cmd)
		},
	}
	cmd.Flags().BoolVar(&available, "available", false, "list what the bank offers at this lab's pinned stack tag, not what is installed")
	cmd.Flags().StringVar(&stack, "stack", "", "read the bank at this release instead of the one this lab is pinned to")
	return cmd
}

func runProvidersInstalled(cmd *cobra.Command) error {
	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}

	if len(rec.Installed) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "no providers installed in %s\n", l.Dir)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSLOT\tSCHEMA\tFROM")
	for _, e := range rec.Installed {
		from := e.StackTag
		if from == "" {
			from = rec.StackTag
		}
		fmt.Fprintf(w, "%s\t%03d\t%d\t%s\n", e.Name, e.Slot, e.SchemaVersion, from)
	}
	return w.Flush()
}

func runProvidersAvailable(cmd *cobra.Command, stackOverride string) error {
	sc, err := resolveStack(cmd, stackOverride)
	if err != nil {
		return err
	}

	b, tree, err := openBank(cmd, sc.BankCommit)
	if err != nil {
		return err
	}
	defer tree.Close()
	names, err := b.List()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMIN STACK\tSUMMARY")
	for _, name := range names {
		m, err := b.Manifest(name)
		if err != nil {
			// One unreadable entry must not hide the rest of the bank, but it
			// must be visible rather than skipped silently.
			fmt.Fprintf(w, "%s\t?\t(unreadable: %v)\n", name, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s%s\n", m.Name, m.MinStack, m.Summary, installability(m, sc))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "\nbank at stack %s (%s)\n", sc.BankTag, shortCommit(sc.BankCommit))
	if sc.LabTag != "" && sc.LabTag != sc.BankTag {
		fmt.Fprintf(out, "this lab runs stack %s, so installability is judged against that\n", sc.LabTag)
	}
	if sc.LabTag == "" {
		fmt.Fprintf(out, "not in a lab, so whether each entry could be installed is unknown\n")
	}
	return nil
}

// installability annotates an entry with the reason it could not be installed
// HERE, judged against what this lab runs rather than against the release the
// entry was published in.
//
// With no lab there is nothing to judge against, and the honest answer is
// silence — a blank column beats one that always says "fine".
func installability(m *manifest.Manifest, sc stackContext) string {
	if err := m.CheckSchemaVersion(); err != nil {
		return "  [needs a newer sal]"
	}
	if sc.LabTag == "" {
		return ""
	}
	if err := m.CheckMinStack(sc.LabTag); err != nil {
		return "  [needs stack " + m.MinStack + "]"
	}
	return ""
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "commit unrecorded"
	}
	return sha
}

func newProvidersAddCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Install a bank entry into this lab",
		Long: "Installs the four files a bank entry may carry — the broker provider, the proxy\n" +
			"addon, the cred-gateway config and an optional lab setup fragment — and records\n" +
			"what it wrote in .sal/installed.json.\n\n" +
			"Order matters and is not negotiable:\n\n" +
			"  1. Refuse a manifest whose schema_version is above what this build supports.\n" +
			"     It may declare a control that would be silently skipped.\n" +
			"  2. Refuse an entry whose min_stack is above this lab's pin, BEFORE writing\n" +
			"     any file. That failure is silent at install and fatal at runtime.\n" +
			"  3. Assign the addon's NNN prefix: the lowest free slot in the manifest's\n" +
			"     band. The bank never bakes a number in.\n" +
			"  4. Prompt for secrets and config separately — different storage, different\n" +
			"     permissions, different prompt. A secret is a 0600 file under the secrets\n" +
			"     directory; config is a value in .env.\n\n" +
			"A route the manifest marks `exposed: false` must not appear in any generated\n" +
			"gateway config. Exposing a token route would hand the lab a reusable secret.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvidersAdd(cmd, args[0], dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run every check and print what would be written, without writing it")
	return cmd
}

func runProvidersAdd(cmd *cobra.Command, name string, dryRun bool) error {
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
	// Which addon numbers are already taken — from the record and from disk,
	// since a stray addon the record does not mention must not have its number
	// reused. Read into a set, never folded into the record: the record says
	// which BANK ENTRIES are installed, and a stack addon is not one.
	occupied := installer.OccupiedSlots(l.Dir, rec)

	// The commit this lab was built from, straight out of its own record —
	// no tag resolution, and no way for a moved tag to change what an
	// existing lab installs from.
	commit, err := commitFor(cmd, rec)
	if err != nil {
		return err
	}
	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return err
	}
	defer tree.Close()

	// Everything that can be refused is refused here, before a byte is
	// written. A half-installed credential path is worse than none.
	plan, err := installer.BuildPlan(b, name, rec, occupied, rec.StackTag)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s — %s\n", plan.Manifest.Name, plan.Manifest.Summary)
	fmt.Fprintf(out, "slot     %03d (%s band)\n", plan.Slot, plan.Manifest.LoadBand)
	fmt.Fprintf(out, "hosts    %s\n", strings.Join(plan.Manifest.Hosts, ", "))
	for _, f := range plan.Files {
		fmt.Fprintf(out, "write    %s\n", f.Dst)
	}
	for _, r := range plan.Manifest.BrokerRoutes {
		state := "not exposed to the lab"
		if r.IsExposed() {
			state = "whitelisted for the lab"
		}
		fmt.Fprintf(out, "route    %s — %s\n", r.Path, state)
	}

	if dryRun {
		fmt.Fprintf(errOut, "\ndry run: every check passed and nothing was written\n")
		return nil
	}

	secretsDir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	values, err := collectValues(cmd, plan, secretsDir)
	if err != nil {
		return err
	}

	entry, err := plan.Apply(l.Dir, secretsDir, rec.StackTag, values)
	if err != nil {
		return err
	}
	rec.Installed = append(rec.Installed, *entry)
	if err := deployment.Save(l.Dir, rec); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "\ninstalled %s into %s\n", entry.Name, l.Name)
	fmt.Fprintf(errOut, "Run `sal up` to restart the lab against it — the broker, proxy and\n"+
		"cred-gateway read these files at startup, so a running lab has not picked them up.\n")

	// Honest about a gap rather than quietly leaving a file nothing reads.
	for _, f := range plan.Files {
		if strings.HasPrefix(f.Dst, "lab/") {
			fmt.Fprintf(errOut, "\nnote: %s was installed but is NOT sourced automatically. The stack's\n"+
				"      lab_setup mechanism assumes the deployment sits inside the workspace, and\n"+
				"      here it deliberately does not. Run it yourself with:\n"+
				"        docker compose -f %s exec lab bash /workspace/../%s\n",
				f.Dst, l.ComposeFile(), f.Dst)
		}
	}
	return nil
}

// collectValues prompts for what the manifest declares, keeping the two kinds
// apart: a secret is a file under the secrets directory read with echo off, a
// config value is a line in .env. Conflating them is how a credential ends up
// somewhere it can be read.
func collectValues(cmd *cobra.Command, plan *installer.Plan, secretsDir string) (installer.Values, error) {
	v := installer.Values{
		Secrets: map[string][]byte{},
		Config:  map[string]string{},
	}
	errOut := cmd.ErrOrStderr()

	for _, s := range plan.Manifest.Secrets {
		path := filepath.Join(secretsDir, s.File)
		if _, err := os.Stat(path); err == nil {
			// Credentials are shared across labs on this machine. Re-prompting
			// for one already stored invites pasting a different value and
			// silently repointing every other lab that uses it.
			fmt.Fprintf(errOut, "using the %s already stored at %s\n", s.File, secretsDir)
			// Reuse skips the write, so it would also skip the mode. A
			// credential that arrived at 0644 by some other route must not
			// stay that way just because sal did not create it.
			tightened, err := installer.EnsureSecretMode(path)
			if err != nil {
				return v, err
			}
			if tightened {
				fmt.Fprintf(errOut, "  tightened %s to 0600\n", s.File)
			}
			continue
		}

		value, err := prompt.ReadSecret(s.Prompt, s.Multiline)
		if err != nil {
			return v, err
		}
		if len(value) == 0 {
			if s.Optional {
				fmt.Fprintf(errOut, "skipped %s (optional)\n", s.Env)
				continue
			}
			return v, fmt.Errorf("%s is required", s.Env)
		}
		v.Secrets[s.Env] = value
	}

	for _, c := range plan.Manifest.Config {
		if c.Help != "" {
			fmt.Fprintf(errOut, "  %s\n", c.Help)
		}
		value, err := prompt.Line(c.Prompt, c.Default)
		if err != nil {
			return v, err
		}
		v.Config[c.Env] = value
	}
	return v, nil
}

func newProvidersCreateCmd() *cobra.Command {
	var template string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Scaffold a new provider from a template",
		Long: "Scaffolds a bank entry locally for a provider the bank does not carry.\n\n" +
			"Note the boundary: the generation constraints for writing a provider from\n" +
			"scratch live in the stack repo's PLAYBOOK.md, which covers exactly the case a\n" +
			"bank of finished entries cannot. This command scaffolds; it does not replace\n" +
			"reading that.",
		Args: cobra.ExactArgs(1),
		RunE: notImplemented,
	}
	cmd.Flags().StringVar(&template, "template", "", "template to scaffold from")
	return cmd
}

func newProvidersRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an installed provider and its files",
		Long: "Removes exactly the files .sal/installed.json records for the entry, rather\n" +
			"than guessing from filenames. Removing a provider narrows the boundary, so it\n" +
			"is safe to get slightly wrong in the cautious direction and dangerous to get\n" +
			"wrong in the other: leave anything unrecorded in place and say so.",
		Args: cobra.ExactArgs(1),
		RunE: notImplemented,
	}
}
