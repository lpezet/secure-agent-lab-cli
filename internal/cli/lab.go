package cli

import "github.com/spf13/cobra"

// bareCommands are the here-and-now conveniences: they act on the lab in the
// current directory. Everything that acts across the machine lives under a
// plural-noun group instead.
//
// They are bare for the same reason gcloud keeps `init` and `info` alongside
// `gcloud storage` — a small set of top-level verbs is worth the exception.
func bareCommands() []*cobra.Command {
	return []*cobra.Command{
		newInitCmd(),
		newUpCmd(),
		newDownCmd(),
		newOpenCmd(),
		newUpgradeCmd(),
		newDriftCmd(),
	}
}

func newInitCmd() *cobra.Command {
	var stackTag string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a lab in this directory",
		Long: "Writes a deployment pinned to a stack tag, replacing \"copy an example directory\n" +
			"and edit it\" as the unit of adoption.\n\n" +
			"The pin is per-project and is what `sal upgrade` rewrites. It is deliberately\n" +
			"not the same thing as the version of sal itself: pinning both together would\n" +
			"mean upgrading your CLI silently moved your security boundary.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
	cmd.Flags().StringVar(&stackTag, "stack", "", "stack tag to pin this lab to (default: the newest sal knows about)")
	return cmd
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Start the lab in this directory",
		Args:  cobra.NoArgs,
		RunE:  notImplemented,
	}
}

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the lab in this directory",
		Args:  cobra.NoArgs,
		RunE:  notImplemented,
	}
}

func newOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open a shell in the lab in this directory",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented,
	}
}

func newUpgradeCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Repin this lab to a newer stack release and update the files it owns",
		Long: "The reason this CLI exists.\n\n" +
			"A deployment holds its own copies of the proxy addons, broker providers and\n" +
			"gateway configs. They are bind-mounted, so they do NOT move when the image tag\n" +
			"does — a lab can repin to a release containing a security fix and keep running\n" +
			"the vulnerable file, because the fix landed in a file it owns a copy of.\n\n" +
			"Repinning without rewriting those files is therefore not an upgrade, and this\n" +
			"command does both.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
	cmd.Flags().StringVar(&to, "to", "", "stack tag to upgrade to (default: the newest sal knows about)")
	return cmd
}

func newDriftCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drift",
		Short: "Report files in this lab that differ from its pinned stack release",
		Long: "Note that scripts/check-drift.sh stays in the stack repo even though this\n" +
			"exists. It is dependency-free bash that works for someone who never installs\n" +
			"sal, and that is worth a little duplication.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
}
