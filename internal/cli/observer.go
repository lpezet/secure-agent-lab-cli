package cli

import "github.com/spf13/cobra"

// newObserverCmd builds the `sal observer` group.
//
// The observer earns a top-level group because it has verbs no other feature
// has: it serves a URL and a stream. A feature that is only enable/disable
// plus a config file stays a `features` citizen — that rule is what keeps
// group-first from degenerating into one top-level group per feature.
//
// Turning the observer off is still `sal features disable observer`.
func newObserverCmd() *cobra.Command {
	group := newGroup("observer", "Read the audit trail")
	group.AddCommand(newObserverOpenCmd(), newObserverTailCmd())
	return group
}

func newObserverOpenCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Print the observer URL, then open a browser",
		Long: "Prints the URL FIRST, then attempts a browser unless --no-open.\n\n" +
			"That order is the whole design. A browser launch fails silently over SSH, in\n" +
			"WSL and inside a dev container — and a dev container is the main path for this\n" +
			"project, not an edge case. Printing first means the URL survives in scrollback\n" +
			"when the launch does nothing.\n\n" +
			"The host port is never chosen by sal. The observer publishes as 127.0.0.1::9000\n" +
			"— empty host port, Docker assigns — and the assignment is read back with\n" +
			"`docker compose port`. That makes collisions structurally impossible instead of\n" +
			"something tracked in a lockfile. The 127.0.0.1 prefix stays: the observer serves\n" +
			"the audit trail over plain HTTP with no auth, and it is only safe because it is\n" +
			"not reachable off the host.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL and do not attempt a browser")
	return cmd
}

func newObserverTailCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Stream the audit trail to the terminal",
		Long: "The answer for a terminal with no browser at all, which is the common case\n" +
			"wherever `observer open` cannot launch anything.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "keep streaming as new lines arrive")
	return cmd
}
