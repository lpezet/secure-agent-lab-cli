package cli

import "github.com/spf13/cobra"

// newLabsCmd builds the `sal labs` group.
//
// The plural noun is doing real work here: these commands act across the
// machine, and the plural is what stops them reading like they act on the lab
// in the current directory the way the bare commands do.
func newLabsCmd() *cobra.Command {
	group := newGroup("labs", "Act on every lab on this machine")
	group.AddCommand(newLabsListCmd(), newLabsDownCmd())
	return group
}

func newLabsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every lab running on this machine",
		Long: "Answers \"what is currently running with my credentials attached?\", which makes\n" +
			"this a control rather than a convenience.\n\n" +
			"One stack per project is deliberate — sharing one would put two projects behind\n" +
			"a single proxy, a single audit trail and a single set of injected credentials,\n" +
			"with an agent working on one project holding credentials scoped for another.\n" +
			"The cost of that choice is six containers per project, and a forgotten lab is\n" +
			"not idle: it is a live credential-injecting proxy with the secrets directory\n" +
			"mounted.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
}

func newLabsDownCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop labs across this machine",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented,
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every running lab on this machine")
	return cmd
}
