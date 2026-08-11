package cli

import "github.com/spf13/cobra"

// newProvidersCmd builds the `sal providers` group.
//
// Everything under here is generic over the bank. `sal providers add <name>`
// must work by someone dropping bank/<name>/ into the stack repo with zero
// changes in this repo — so no command here may branch on which provider it
// was handed. internal/invariants holds the test that says so.
func newProvidersCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "providers",
		Short: "Install and manage credential providers from the bank",
	}
	group.AddCommand(
		newProvidersListCmd(),
		newProvidersAddCmd(),
		newProvidersCreateCmd(),
		newProvidersRemoveCmd(),
	)
	return group
}

func newProvidersListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List providers installed in this lab",
		Args:  cobra.NoArgs,
		RunE:  notImplemented,
	}
	cmd.Flags().BoolVar(&all, "available", false, "list what the bank offers at this lab's pinned stack tag, not what is installed")
	return cmd
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
