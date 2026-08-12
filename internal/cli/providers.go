package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
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
	sc, err := resolveStack(stackOverride)
	if err != nil {
		return err
	}

	b, err := openBank(cmd, sc.BankTag)
	if err != nil {
		return err
	}
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
	fmt.Fprintf(out, "\nbank at stack %s (%s)\n", b.Tag(), shortCommit(b.Commit()))
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
		RunE: notImplemented,
	}
	return cmd
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
