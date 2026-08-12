package cli

import "github.com/spf13/cobra"

// newSecretsCmd builds the `sal secrets` group.
//
// Secrets live at ~/.config/secure-agent-lab/secrets/, 0700 on the directory
// and 0600 on the files. The broker mounts THAT directory and never its
// parent: the parent also holds config, the install record and a bank cache,
// none of which belongs anywhere near the broker.
func newSecretsCmd() *cobra.Command {
	group := newGroup("secrets", "Store credentials for this machine's labs")
	group.AddCommand(newSecretsSetCmd(), newSecretsListCmd())
	return group
}

func newSecretsSetCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Store a credential, read from the terminal with echo off",
		Long: "Takes the name of what to set and nothing else. The value is read from the\n" +
			"terminal with echo off, and a non-terminal stdin is refused rather than read.\n\n" +
			"There is no flag to pass the value, and there will not be one. An argv is in\n" +
			"shell history, in ps, and in any process listing the agent can run — and a pipe\n" +
			"is an argv one process upstream.\n\n" +
			"Where a provider has genuinely different credential kinds, --type records which\n" +
			"one this is rather than leaving it to be re-derived from the value's shape\n" +
			"later. Guessing a credential's kind from its prefix is a bet on a vendor's\n" +
			"formatting; writing the decision down is not.",
		Args: cobra.ExactArgs(1),
		RunE: notImplemented,
	}
	cmd.Flags().StringVar(&kind, "type", "", "credential kind, when the provider has more than one; detected from the value's prefix if unset")
	return cmd
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored credentials by name, never by value",
		Args:  cobra.NoArgs,
		RunE:  notImplemented,
	}
}
