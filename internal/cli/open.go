package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/compose"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

// labService is the compose service the agent works in. Named here because
// `sal open` execs into it by name and the template defines it by name, and
// the two must not be able to disagree.
const labService = "lab"

func newOpenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "open [command ...]",
		Short: "Open a shell in this project's lab",
		Long: "Opens a shell inside the lab container — the one on the internal network,\n" +
			"whose only way out is the proxy, and whose traffic is in the audit trail.\n" +
			"That last part is the point: a shell somewhere else may look the same and is\n" +
			"not the same thing at all.\n\n" +
			"With arguments, runs them instead of a shell. Flags after the command are the\n" +
			"command's, not sal's.",
		Args: cobra.ArbitraryArgs,
		RunE: runOpen,
	}
	// So `sal open ls -la` passes -la to ls rather than having sal reject it.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func runOpen(cmd *cobra.Command, args []string) error {
	l, r, err := runnerFor(cmd)
	if err != nil {
		return err
	}

	// exec needs the terminal, which runnerFor does not wire up: every other
	// command sal runs is one that should never read from stdin.
	r.Stdin = cmd.InOrStdin()

	running, err := serviceRunning(cmd, r)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("lab %q is not running, so there is nothing to open a shell in.\n"+
			"Start it with `sal up`", l.Name)
	}

	warnAboutForeignDevContainer(cmd, l)

	return r.Run(cmd.Context(), append([]string{"exec", labService}, command(args)...)...)
}

// command is what to run inside the lab.
//
// The default falls back from bash to sh inside the container rather than sal
// probing first. Which shells a lab image has is the image's business — the
// stack's own is Debian-based, but a lab_setup fragment or a hand-written
// lab/Dockerfile can make it anything, and sal holding an opinion about that
// would be wrong the moment someone changes their base image.
//
// The probe is `command -v`, and the redirect belongs to the PROBE — never to
// bash itself. This was `exec bash 2>/dev/null || exec sh`, which silenced a
// "not found" message and, in doing so, handed the operator a shell with no
// prompt: bash decides it is interactive by testing that stdin AND STDERR are
// terminals, so `2>/dev/null` starts it non-interactive. A non-interactive
// bash still reads and runs what is typed, which is why it looks like a
// working shell — with no prompt, no readline, no history and no job control,
// and no clue as to why. Redirecting bash's stderr here is never right.
func command(args []string) []string {
	if len(args) > 0 {
		return args
	}
	return []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; fi; exec sh"}
}

// serviceRunning reports whether the lab service has a container up.
//
// Asked before exec so that a stopped lab gets a sentence naming `sal up`,
// rather than docker's own message about a service with no container — which
// is accurate and tells nobody what to do about it.
func serviceRunning(cmd *cobra.Command, r *compose.Runner) (bool, error) {
	quiet := *r
	quiet.Stderr = io.Discard

	out, err := quiet.Output(cmd.Context(), "ps", "--quiet", labService)
	if err != nil {
		// A compose file that cannot be read is a different failure, and it
		// has already been reported on the stderr this call discarded — so
		// say which question could not be answered.
		return false, fmt.Errorf("cannot tell whether lab service %q is running: %w", labService, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// warnAboutForeignDevContainer says so when VS Code is running a dev container
// for this project that is not this lab.
//
// This is the case the ".devcontainer" question in CLAUDE.md is about, and the
// warning is the half of it that cannot wait: a dev container the operator
// brought themselves is NOT on the lab network, so it does not go out through
// the proxy and nothing it does reaches the audit trail. A shell in it looks
// exactly like a shell in the lab. Saying nothing would let someone believe
// the boundary was around them when it was not.
//
// Never fatal, and never a redirect: `sal open` opens THIS project's lab,
// which is the one thing that is always the right answer.
func warnAboutForeignDevContainer(cmd *cobra.Command, l *lab.Lab) {
	if _, err := os.Stat(filepath.Join(l.ProjectDir, devContainerDir)); err != nil {
		return
	}

	// The extension labels each container with the folder it was started for.
	forProject := "devcontainer.local_folder=" + l.ProjectDir
	all, err := compose.ContainerNames(cmd.Context(), forProject)
	if err != nil || len(all) == 0 {
		return
	}

	// The same question again, narrowed to this lab's compose project: what is
	// left over is a dev container that is not part of it. A dev container
	// that IS the lab's own service is not worth a word — it is exactly where
	// this command is about to put you.
	ours, err := compose.ContainerNames(cmd.Context(), forProject, "com.docker.compose.project="+l.Name)
	if err != nil {
		return
	}
	foreign := notIn(all, ours)
	if len(foreign) == 0 {
		return
	}

	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: %s is also running for this project (%s), and it is not this lab.\n"+
			"      A shell there does not go out through the proxy and nothing it does\n"+
			"      reaches the audit trail. Opening the lab instead.\n",
		strings.Join(foreign, ", "), devContainerDir)
}

// devContainerDir is the directory whose presence makes the question above
// worth asking at all.
const devContainerDir = ".devcontainer"

func notIn(all, exclude []string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	var rest []string
	for _, a := range all {
		if !skip[a] {
			rest = append(rest, a)
		}
	}
	return rest
}
