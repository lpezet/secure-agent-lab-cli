package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/compose"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/version"
)

// bareCommands are the here-and-now conveniences: they act on the lab for the
// current directory. Everything that acts across the machine lives under a
// plural-noun group instead.
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
		Short: "Create a lab for this project",
		Long: "Creates a deployment for the project in this directory, replacing \"copy an\n" +
			"example directory and edit it\" as the unit of adoption.\n\n" +
			"The deployment does NOT live in your project. It goes under\n" +
			"~/.config/secure-agent-lab/labs/, and the project keeps only a committable\n" +
			"pointer at .sal/lab.json. That is a boundary property: the agent works in the\n" +
			"project, so a deployment kept there is one the agent can edit — and the proxy\n" +
			"addons, broker providers and gateway configs are exactly what it would want to\n" +
			"edit. Out of the workspace, they are not merely unwritable but invisible.\n\n" +
			"The lab starts virgin: six services, no providers, no credentials. The broker\n" +
			"answers 404 on every credential route and cred-gateway denies everything but\n" +
			"/healthz until `sal providers add` puts something there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, stackTag)
		},
	}
	cmd.Flags().StringVar(&stackTag, "stack", "", "stack release to pin this lab to (default: the newest sal knows about)")
	return cmd
}

func runInit(cmd *cobra.Command, stackTag string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	// Canonical, not merely absolute: the same path derives the lab's identity
	// and becomes the /workspace mount, and those two must not be able to
	// disagree about which directory this is.
	projectDir, err := lab.CanonicalDir(cwd())
	if err != nil {
		return err
	}

	// Only this directory, not its ancestors: a nested project is a mistake
	// worth warning about, but an ancestor's lab must not block creating one
	// here.
	pointerPath := filepath.Join(projectDir, lab.PointerDir, lab.PointerFile)
	if _, err := os.Stat(pointerPath); err == nil {
		return fmt.Errorf("%s already exists; this project already has a lab", pointerPath)
	}
	if parent, _, err := lab.Find(filepath.Dir(projectDir)); err == nil {
		fmt.Fprintf(errOut, "note: %s already has lab %q. One stack per project is deliberate,\n"+
			"      but two labs nested inside one tree is usually not what you meant.\n",
			parent.ProjectDir, parent.Name)
	}

	if stackTag == "" {
		stackTag = version.DefaultStack
		fmt.Fprintf(errOut, "pinning to stack %s, the newest release this sal knows about; pass --stack to choose another\n", stackTag)
	}

	name, err := lab.NameFor(projectDir)
	if err != nil {
		return err
	}
	labsRoot, err := config.LabsDir()
	if err != nil {
		return err
	}
	labDir := filepath.Join(labsRoot, name)
	if _, err := os.Stat(labDir); err == nil {
		return fmt.Errorf("%s already exists but this project has no pointer to it; "+
			"remove that directory or restore .sal/lab.json rather than having sal guess which is right", labDir)
	}

	// Resolve the tag to a commit before creating anything. It validates that
	// the release exists — better to fail here than to leave a deployment
	// pinned to a tag that does not — and a tag is mutable, so the commit is
	// what records which boundary this lab was actually built from.
	commit := ""
	offline, _ := cmd.Flags().GetBool("offline")
	if offline {
		fmt.Fprintf(errOut, "warning: --offline, so %s is unverified and no commit is recorded\n", stackTag)
	} else {
		commit, err = bank.DefaultSource().ResolveTag(cmd.Context(), stackTag)
		if err != nil {
			return err
		}
	}

	// The broker mounts this. If it does not exist when the container starts,
	// Docker creates it root-owned, and every later `sal secrets set` fails on
	// a directory the user cannot write.
	secretsDir, err := config.SecretsDir()
	if err != nil {
		return err
	}

	if err := createLabTree(labDir); err != nil {
		return err
	}

	var rendered bytes.Buffer
	if err := compose.Render(&rendered, compose.Data{
		ProjectName: name,
		ProjectDir:  projectDir,
		SecretsDir:  secretsDir,
		StackTag:    stackTag,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(labDir, "compose.yaml"), rendered.Bytes(), 0o600); err != nil {
		return err
	}

	// Both start empty and are filled by `sal providers add` from manifests.
	// Compose requires the files to exist, and keeping them separate is what
	// stops the lab container receiving the broker's environment.
	if err := writeEnvFile(filepath.Join(labDir, ".env"),
		"Broker and proxy configuration. Written by `sal providers add` from each\nprovider's manifest; a virgin lab needs nothing here."); err != nil {
		return err
	}
	if err := writeEnvFile(filepath.Join(labDir, "lab.env"),
		"Environment for the lab container only, from each provider's lab_env.\nSeparate from .env so the lab never receives the broker's environment."); err != nil {
		return err
	}

	if err := deployment.Save(labDir, &deployment.Record{
		StackTag:    stackTag,
		StackCommit: commit,
		Installed:   []deployment.Entry{},
	}); err != nil {
		return err
	}
	if err := lab.WritePointer(projectDir, lab.Pointer{Name: name, StackTag: stackTag}); err != nil {
		return err
	}

	fmt.Fprintf(out, "lab      %s\n", name)
	fmt.Fprintf(out, "at       %s\n", labDir)
	fmt.Fprintf(out, "stack    %s", stackTag)
	if commit != "" {
		fmt.Fprintf(out, " (%s)", shortCommit(commit))
	}
	fmt.Fprintf(out, "\nproject  %s\n", projectDir)

	fmt.Fprintf(errOut, "\nNext: `sal providers add <name>` to give it a credential path, then `sal up`.\n"+
		"The first `sal up` builds five images from the stack repo and takes a few minutes.\n")
	return nil
}

// createLabTree makes the deployment directory and the three mount points a
// bank entry installs into. 0700 throughout: what is in here decides which
// code runs behind the credential boundary.
func createLabTree(labDir string) error {
	for _, dir := range []string{
		labDir,
		filepath.Join(labDir, "broker"),
		filepath.Join(labDir, "proxy"),
		filepath.Join(labDir, "cred-gateway"),
		filepath.Join(labDir, deployment.Dir),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func writeEnvFile(path, comment string) error {
	var b bytes.Buffer
	for _, line := range splitLines(comment) {
		fmt.Fprintf(&b, "# %s\n", line)
	}
	return os.WriteFile(path, b.Bytes(), 0o600)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range bytes.Split([]byte(s), []byte("\n")) {
		out = append(out, string(l))
	}
	return out
}

func newUpCmd() *cobra.Command {
	var build bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start this project's lab",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUp(cmd, build)
		},
	}
	cmd.Flags().BoolVar(&build, "build", false, "rebuild images before starting")
	return cmd
}

func runUp(cmd *cobra.Command, build bool) error {
	l, r, err := runnerFor(cmd)
	if err != nil {
		return err
	}

	args := []string{"up", "-d", "--wait"}
	if build {
		args = append(args, "--build")
	}

	started := time.Now()
	if err := r.Run(cmd.Context(), args...); err != nil {
		return err
	}

	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "\nlab %s up in %s\n", l.Name, time.Since(started).Round(time.Second))

	// Printed first and unconditionally, because a browser launch fails
	// silently over SSH, in WSL and inside a dev container.
	if url, err := r.ObserverURL(cmd.Context()); err == nil && url != "" {
		fmt.Fprintf(out, "observer %s\n", url)
	}
	return nil
}

func newDownCmd() *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop this project's lab",
		Long: "Stops the lab and removes its containers. The audit trail and the proxy CA\n" +
			"survive, because they are the two things you are most likely to want after\n" +
			"stopping. --volumes destroys both.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDown(cmd, volumes)
		},
	}
	cmd.Flags().BoolVar(&volumes, "volumes", false, "also delete the audit trail and the proxy CA")
	return cmd
}

func runDown(cmd *cobra.Command, volumes bool) error {
	l, r, err := runnerFor(cmd)
	if err != nil {
		return err
	}

	args := []string{"down"}
	if volumes {
		// Destroying the audit trail is the one irreversible thing `down`
		// can do, and the trail is the record of what the agent did.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"--volumes deletes lab %s's audit trail and its proxy CA. The trail is the\n"+
				"record of everything the agent did through this lab, and it is not recoverable.\n", l.Name)
		ok, err := confirm(cmd, "Delete them?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cancelled")
		}
		args = append(args, "--volumes")
	}
	return r.Run(cmd.Context(), args...)
}

// runnerFor resolves this directory's lab and prepares a compose runner,
// failing early and clearly on the two things most likely to be wrong.
func runnerFor(cmd *cobra.Command) (*lab.Lab, *compose.Runner, error) {
	l, _, err := lab.Find(cwd())
	if err != nil {
		return nil, nil, err
	}
	if !l.Exists() {
		return nil, nil, fmt.Errorf("lab %q is recorded for this project but %s has no compose file; run `sal init`", l.Name, l.Dir)
	}
	if err := compose.Available(cmd.Context()); err != nil {
		return nil, nil, err
	}
	return l, &compose.Runner{
		File:   l.ComposeFile(),
		Stdout: cmd.OutOrStdout(),
		Stderr: cmd.ErrOrStderr(),
	}, nil
}

func newOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open a shell in this project's lab",
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
			"gateway configs. They are bind-mounted, so they do NOT move when the pinned\n" +
			"release does — a lab can repin to a release containing a security fix and keep\n" +
			"running the vulnerable file, because the fix landed in a file it owns a copy of.\n\n" +
			"Repinning without rewriting those files is therefore not an upgrade, and this\n" +
			"command does both.",
		Args: cobra.NoArgs,
		RunE: notImplemented,
	}
	cmd.Flags().StringVar(&to, "to", "", "stack release to upgrade to (default: the newest sal knows about)")
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
