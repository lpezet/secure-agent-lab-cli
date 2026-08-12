package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/stackver"
	"github.com/lpezet/secure-agent-lab-cli/internal/version"
)

// Execute runs the CLI, returning an error for main to report.
func Execute() error {
	return NewRootCmd().Execute()
}

// NewRootCmd builds the command tree.
//
// The grammar is gcloud-shaped: group, then verb, then positional, with a
// small set of bare top-level commands alongside. Two rules keep it from
// drifting:
//
//   - Lifecycle verbs are uniform, so they live in one group. Turning the
//     observer off is `sal features disable observer`, never `sal observer
//     remove` — otherwise every feature owns a copy of enable/disable/list and
//     there is no single place to answer "what is on?".
//   - A feature earns a top-level group only when it has verbs no other
//     feature has. The observer earns one because `open` and `tail` are
//     specific to something that serves a URL and a stream.
func NewRootCmd() *cobra.Command {
	var showVersion bool

	root := &cobra.Command{
		Use:   "sal",
		Short: "Set up, maintain and operate a secure-agent-lab",
		Long: "sal installs and updates the files a secure-agent-lab deployment owns.\n\n" +
			"The stack versions the security boundary by tag, but a deployment holds its own\n" +
			"copies of the proxy addons, broker providers and gateway configs — so repinning\n" +
			"to a release with a security fix does not, by itself, apply it. A tool that\n" +
			"installed those files is a tool that can update them.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				return runVersion(cmd)
			}
			if len(args) > 0 {
				return unknownSubcommand(cmd, args[0])
			}
			return cmd.Help()
		},
	}

	root.PersistentFlags().BoolVar(&showVersion, "version", false, "print the sal version and the stack version it is managing")

	// Both concern how the bank is obtained, and every command that touches it
	// needs them, so they are global rather than repeated per command.
	root.PersistentFlags().Bool("offline", false, "never reach the network; use only what is already cached")
	root.PersistentFlags().Bool("refresh", false, "re-resolve the stack tag even if it is cached, which is how a moved tag gets noticed")

	root.AddCommand(
		newProvidersCmd(),
		newSecretsCmd(),
		newFeaturesCmd(),
		newObserverCmd(),
		newLabsCmd(),
		newVersionCmd(),
	)
	root.AddCommand(bareCommands()...)

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sal version and the stack version it is managing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVersion(cmd)
		},
	}
}

// runVersion prints BOTH versions, always.
//
// This is contract item 5, and it is not cosmetic: sal and the stack are
// tagged on separate lines, so a single version number cannot answer "how far
// behind is my security boundary?". Printing one of them would put the
// ambiguity back that splitting the repos removed.
func runVersion(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	self := version.CLI()
	if c := version.Commit(); c != "" {
		self += " (" + c + ")"
	}
	fmt.Fprintf(out, "sal          %s\n", self)

	l, ptr, err := lab.Find(cwd())
	switch {
	case errors.Is(err, lab.ErrNoLab):
		fmt.Fprintf(out, "stack        unknown — this directory has no lab\n")
		return nil
	case err != nil:
		return err
	}

	// Two records name a version, and they answer different questions. The
	// deployment's installed.json says what was actually rendered; the
	// project's lab.json is committable and may have arrived from a teammate
	// or a branch. The deployment wins, and a disagreement is worth saying
	// out loud rather than resolving silently.
	tag, commit := ptr.StackTag, ""
	rec, recErr := deployment.Load(l.Dir)
	if recErr == nil && rec.StackTag != "" {
		tag = rec.StackTag
		commit = rec.StackCommit
	}
	if tag == "" {
		tag = "unrecorded"
	}

	fmt.Fprintf(out, "stack        %s", tag)
	if commit != "" {
		fmt.Fprintf(out, " (%s)", commit)
	}
	fmt.Fprintf(out, "\nproject      %s\n", l.ProjectDir)
	fmt.Fprintf(out, "lab          %s\n", l.Dir)

	errOut := cmd.ErrOrStderr()
	if !l.Exists() {
		fmt.Fprintf(errOut, "\nwarning: %s names lab %q, but no deployment exists there.\n"+
			"         Run `sal init` to create it, or delete %s to forget it.\n",
			filepath.Join(l.ProjectDir, lab.PointerDir, lab.PointerFile), l.Name, lab.PointerDir)
	} else if recErr == nil && ptr.StackTag != "" && rec.StackTag != "" && ptr.StackTag != rec.StackTag {
		fmt.Fprintf(errOut, "\nwarning: this project asks for stack %s but its lab was built from %s.\n"+
			"         Run `sal upgrade` to bring the lab to what the project asks for.\n",
			ptr.StackTag, rec.StackTag)
	}

	warnIfStackTooOld(cmd, tag)
	return nil
}

// warnIfStackTooOld is the WARN-level check of the three version checks, and
// the only one this repo owns. The other two — a manifest's schema_version
// above what this build supports, and a manifest's min_stack above the
// deployment's pin — are refusals, and they live in internal/manifest. Keeping
// them apart is deliberate: each is wrong at the others' severity.
func warnIfStackTooOld(cmd *cobra.Command, tag string) {
	if tag == "" {
		return
	}
	have, err := stackver.Parse(tag)
	if err != nil {
		return
	}
	min, err := stackver.Parse(version.MinimumStack)
	if err != nil {
		return
	}
	if have.Less(min) {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"\nwarning: this lab is pinned to %s, below the %s that sal expects.\n"+
				"         Below %s a bank manifest carries no schema_version, so sal cannot tell\n"+
				"         whether it understands one it is reading. Run `sal upgrade` when you can.\n",
			have.Tag(), min.Tag(), min.Tag())
	}
}

// newGroup builds a command whose only job is to group others.
//
// Cobra's default when a group is handed something that is not one of its
// subcommands is to print help and exit 0. That is the "appeared to succeed"
// failure this CLI cannot afford. `sal observer disable` is a plausible thing
// to type — it is deliberately not a command, because lifecycle verbs live in
// `features` — and a script that ran it and saw exit 0 would carry on
// believing the observer had been turned off.
func newGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownSubcommand(cmd, args[0])
			}
			// A bare group with no arguments is a request for help, which is
			// a fair thing to ask for and not an error.
			return cmd.Help()
		},
	}
}

func unknownSubcommand(cmd *cobra.Command, arg string) error {
	return fmt.Errorf("unknown command %q for %q — run '%s --help' to see what it does have",
		arg, cmd.CommandPath(), cmd.CommandPath())
}

// notImplemented is what an unwritten command does. It is an error rather than
// a no-op with a friendly message so that a script calling it fails instead of
// appearing to succeed.
func notImplemented(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("%s: not implemented yet", cmd.CommandPath())
}

// cwd is the directory commands act on. Bare commands act on the lab here;
// the plural-noun groups act across the machine.
func cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}
