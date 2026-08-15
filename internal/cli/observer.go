package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/browser"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/observer"
)

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
			"The URL is the only thing on stdout, so `sal observer open --no-open` is also\n" +
			"how a script gets it. Everything else this prints is on stderr.\n\n" +
			"The host port is never chosen by sal. The observer publishes as 127.0.0.1::9000\n" +
			"— empty host port, Docker assigns — and the assignment is read back with\n" +
			"`docker compose port`. That makes collisions structurally impossible instead of\n" +
			"something tracked in a lockfile. The 127.0.0.1 prefix stays: the observer serves\n" +
			"the audit trail over plain HTTP with no auth, and it is only safe because it is\n" +
			"not reachable off the host.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runObserverOpen(cmd, noOpen)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL and do not attempt a browser")
	return cmd
}

func runObserverOpen(cmd *cobra.Command, noOpen bool) error {
	url, err := observerURL(cmd)
	if err != nil {
		return err
	}
	printAndOpen(cmd.OutOrStdout(), cmd.ErrOrStderr(), url, noOpen)
	return nil
}

// printAndOpen writes the URL and then, unless told not to, asks something to
// display it.
//
// Split out from the command because the order is the whole design and it is
// the one thing a txtar script cannot check: it captures stdout and stderr
// separately, so a version that launched first and printed afterwards would
// pass every assertion there. See TestTheURLComesBeforeAnythingThatCanFail.
//
// Nothing here returns an error. The URL is the answer and it has already been
// given; exiting non-zero because a browser did not start would report that a
// command which did its job did not.
func printAndOpen(out, errOut io.Writer, url string, noOpen bool) {
	fmt.Fprintln(out, url)
	if noOpen {
		return
	}

	used, err := browser.Open(url)
	if err != nil {
		fmt.Fprintf(errOut, "note: %v, so nothing was launched. The URL above is the whole answer.\n", err)
		return
	}
	fmt.Fprintf(errOut, "opening with `%s`. If no window appears — over SSH, in WSL, or inside a\n"+
		"dev container it often does not — the URL above still works from the host.\n", used)
}

func newObserverTailCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Stream the audit trail to the terminal",
		Long: "The answer for a terminal with no browser at all, which is the common case\n" +
			"wherever `observer open` cannot launch anything.\n\n" +
			"Every connection replays what the observer still holds — a few hundred recent\n" +
			"events — and then stays open. --follow=false prints that history and exits;\n" +
			"since the stream carries no marker between the replay and what happens next,\n" +
			"\"the history\" means everything that arrived before the stream went quiet.\n\n" +
			"Events are rendered by shape: the timestamp, service and event name that every\n" +
			"writer in the stack emits, then whatever else the line carried, as key=value.\n" +
			"sal does not know what any provider's events mean and does not need to.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runObserverTail(cmd, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "keep streaming as new lines arrive")
	return cmd
}

func runObserverTail(cmd *cobra.Command, follow bool) error {
	url, err := observerURL(cmd)
	if err != nil {
		return err
	}

	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "tailing %s%s\n", url, observer.EventsPath)

	// The trail itself is the only thing on stdout, so it can be piped into
	// grep without the framing above coming with it.
	n, err := observer.Stream(cmd.Context(), url, cmd.OutOrStdout(), observer.Options{Follow: follow})
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintf(errOut, "nothing in the trail yet: this lab has not been through the proxy or the broker.\n")
	}
	return nil
}

// observerURL resolves this directory's lab and asks Docker which host port it
// assigned to the observer.
//
// sal never picks that port — the compose file publishes 127.0.0.1::9000 and
// lets Docker choose — so there is nothing to look up anywhere, and a lab that
// is not running has no answer to give at all.
func observerURL(cmd *cobra.Command) (string, error) {
	l, r, err := runnerFor(cmd)
	if err != nil {
		return "", err
	}

	// Docker's own complaint about a service with no container is discarded
	// in favour of the sentence below, which says what to do about it.
	quiet := *r
	quiet.Stderr = io.Discard

	// An error and an empty answer are the same finding here. Whether
	// `docker compose port` complains about a service with no container or
	// prints nothing at all has varied between compose versions, and the
	// daemon itself was already checked by runnerFor — so anything that comes
	// back other than a port means the lab is not up.
	url, err := quiet.ObserverURL(cmd.Context())
	if err != nil || url == "" {
		return "", notRunning(l)
	}
	return url, nil
}

func notRunning(l *lab.Lab) error {
	return fmt.Errorf("lab %q has no observer port published, which means it is not running.\n"+
		"Start it with `sal up`, or `sal labs list` to see what is running on this machine", l.Name)
}
