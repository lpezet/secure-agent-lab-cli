package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/compose"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/installer"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/prompt"
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
	if local := stackDir(cmd); local != "" {
		fmt.Fprintf(errOut, "reading stack content from %s, so %s is unverified and no commit is recorded\n", local, stackTag)
	} else {
		commit, err = bank.DefaultSource().ResolveTag(cmd.Context(), stackTag)
		if err != nil {
			return err
		}
	}

	// The broker mounts this. If it does not exist when the container starts,
	// Docker creates it root-owned, and every later `sal secrets set` fails on
	// a directory the user cannot write.
	if _, err := config.SecretsDir(); err != nil {
		return err
	}

	// Fetch the stack's own proxy addons BEFORE creating anything, so a lab is
	// never left existing without them.
	//
	// This is not optional decoration. 000_policy.py is what stops the proxy
	// forwarding to the broker — and the proxy sits on both networks, so
	// without it the lab can ask the proxy to fetch http://broker:8080/<any
	// route> and walk straight around the cred-gateway whitelist, including
	// the routes a manifest marks exposed:false. A deployment missing it has a
	// credential path with no gate on it.
	addons, err := fetchBaseAddons(cmd, commit)
	if err != nil {
		return err
	}
	defer addons.Close()

	if err := createLabTree(labDir); err != nil {
		return err
	}
	installed, err := copyAddons(addons.Dir, filepath.Join(labDir, "proxy"))
	if err != nil {
		return err
	}

	// The same renderer upgrade uses, so the two cannot drift apart on what a
	// deployment's compose file should look like.
	if err := renderCompose(&lab.Lab{Name: name, Dir: labDir, ProjectDir: projectDir}, stackTag); err != nil {
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
		ProjectDir:  projectDir,
		Installed:   []deployment.Entry{},
		BaseAddons:  installed,
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
	for _, a := range installed {
		fmt.Fprintf(out, "addon    %s\n", a)
	}

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

// fetchBaseAddons obtains the stack's own proxy addons at the pinned release.
//
// Refused rather than skipped when unavailable: a lab created without the
// policy addon has a cred-gateway whitelist that can be walked around, and
// "created but not safe yet" is not a state worth being able to reach.
func fetchBaseAddons(cmd *cobra.Command, commit string) (*bank.Tree, error) {
	tree, err := bank.FetchTree(cmd.Context(), commit, bank.AddonsSubtree, bankOptions(cmd))
	if err != nil {
		return nil, fmt.Errorf("cannot obtain the stack's proxy addons, without which the lab would have "+
			"no barrier between the lab container and the broker: %w", err)
	}
	return tree, nil
}

// copyAddons installs every addon the stack ships, without judging which
// matter.
//
// Deliberately not a filtered list. The allowlist addon is inert when its file
// is absent (it says so itself: every destination is permitted, with a warning
// at startup), so installing all of them costs nothing and means sal does not
// hold an opinion about which of the stack's addons are load-bearing — an
// opinion that would silently go stale the moment the stack adds one.
func copyAddons(srcDir, dstDir string) ([]string, error) {
	items, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, err
	}
	var installed []string
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".py") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(srcDir, it.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dstDir, it.Name()), body, 0o644); err != nil {
			return nil, err
		}
		installed = append(installed, it.Name())
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("the stack's addon directory at %s is empty", srcDir)
	}
	sort.Strings(installed)
	return installed, nil
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
	var (
		to     string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Move this lab to a newer stack release and rewrite the files it owns",
		Long: "The reason this CLI exists.\n\n" +
			"A deployment holds its own copies of the proxy addons, broker providers and\n" +
			"gateway configs. They are bind-mounted, so they do NOT move when the pinned\n" +
			"release does — a lab can repin to a release containing a security fix and keep\n" +
			"running the vulnerable file, because the fix landed in a file it owns a copy of.\n" +
			"Repinning without rewriting those files is therefore not an upgrade.\n\n" +
			"So this reinstalls every recorded provider from the new release, keeping the\n" +
			"slot each was assigned, installs the new release's own proxy addons, DELETES\n" +
			"files the new versions no longer ship, and re-renders compose.yaml.\n\n" +
			"Every provider is checked before anything is written, and one that cannot make\n" +
			"the move refuses the whole upgrade. Half a deployment on each of two releases\n" +
			"is a boundary nobody can describe.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd, to, dryRun)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "stack release to move to (default: the newest sal knows about)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change, and change nothing")
	return cmd
}

func runUpgrade(cmd *cobra.Command, to string, dryRun bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	if !l.Exists() {
		return fmt.Errorf("lab %q has no deployment at %s; run `sal init`", l.Name, l.Dir)
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}

	if to == "" {
		to = version.DefaultStack
	}

	toCommit := ""
	if local := stackDir(cmd); local == "" {
		toCommit, err = bank.DefaultSource().ResolveTag(cmd.Context(), to)
		if err != nil {
			return err
		}
		// A tag can move, so the commit is what says whether this is actually
		// a change. Same tag with a different commit IS an upgrade.
		if to == rec.StackTag && toCommit == rec.StackCommit {
			fmt.Fprintf(errOut, "lab %s is already at %s (%s); nothing to do\n", l.Name, to, shortCommit(toCommit))
			return nil
		}
	}

	b, tree, err := openBank(cmd, toCommit)
	if err != nil {
		return err
	}
	defer tree.Close()

	addons, err := fetchBaseAddons(cmd, toCommit)
	if err != nil {
		return err
	}
	defer addons.Close()

	plan, err := installer.BuildUpgradePlan(b, addons.Dir, rec, to, toCommit)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "from     %s (%s)\n", plan.FromTag, shortCommit(plan.FromCommit))
	fmt.Fprintf(out, "to       %s (%s)\n", plan.ToTag, shortCommit(plan.ToCommit))
	for _, e := range plan.Entries {
		fmt.Fprintf(out, "\nprovider %s (slot %03d, unchanged)\n", e.Plan.Manifest.Name, e.Plan.Slot)
		for _, f := range e.Plan.Files {
			fmt.Fprintf(out, "  rewrite %s\n", f.Dst)
		}
		for _, stale := range e.Stale {
			// Worth its own word: a gateway config left behind keeps
			// whitelisting a route the entry no longer exposes.
			fmt.Fprintf(out, "  DELETE  %s (not shipped at %s)\n", stale, plan.ToTag)
		}
	}
	if len(plan.Entries) > 0 {
		fmt.Fprintln(out)
	}
	for _, a := range plan.BaseAddons {
		fmt.Fprintf(out, "addon    %s\n", a)
	}
	for _, a := range plan.StaleAddons {
		fmt.Fprintf(out, "DELETE   %s (not shipped at %s)\n", a, plan.ToTag)
	}

	if dryRun {
		fmt.Fprintf(errOut, "\ndry run: every provider can make the move and nothing was changed\n")
		return nil
	}

	secretsDir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	values, err := collectUpgradeValues(cmd, plan, l.Dir)
	if err != nil {
		return err
	}

	newRec, err := plan.Apply(l.Dir, secretsDir, values)
	if err != nil {
		return err
	}

	// Set from what was OBSERVED — l.ProjectDir is the directory whose pointer
	// actually named this lab — rather than carried forward from the old
	// record, which is only a claim made whenever the lab was created. It also
	// means a lab from before the field existed gains one here, which is what
	// the "unrecorded" line in `sal labs list` tells the operator to do.
	newRec.ProjectDir = l.ProjectDir

	// Re-render last: if anything above failed, the compose file still
	// describes the release the files on disk came from.
	if err := renderCompose(l, to); err != nil {
		return err
	}
	if err := deployment.Save(l.Dir, newRec); err != nil {
		return err
	}
	if err := lab.WritePointer(l.ProjectDir, lab.Pointer{Name: l.Name, StackTag: to}); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "\nupgraded %s to %s\n", l.Name, to)
	warnMissingSecrets(cmd, plan, secretsDir)
	fmt.Fprintf(errOut, "Run `sal up --build` to rebuild the images and restart against the new release.\n"+
		"Until then the containers are still running the old one.\n")
	return nil
}

// renderCompose rewrites compose.yaml for a release.
func renderCompose(l *lab.Lab, stackTag string) error {
	rendered, err := renderComposeBytes(l, stackTag)
	if err != nil {
		return err
	}
	return os.WriteFile(l.ComposeFile(), rendered, 0o600)
}

// renderComposeBytes renders without writing, which is what lets `sal drift`
// compare a deployment's compose file against the one sal would produce for it
// — using the same renderer, so the two cannot drift apart on what a
// deployment's compose file should look like.
func renderComposeBytes(l *lab.Lab, stackTag string) ([]byte, error) {
	secretsDir, err := config.SecretsDir()
	if err != nil {
		return nil, err
	}
	_, labDockerfile := os.Stat(filepath.Join(l.Dir, "lab", "Dockerfile"))

	var rendered bytes.Buffer
	if err := compose.Render(&rendered, compose.Data{
		ProjectName:   l.Name,
		ProjectDir:    l.ProjectDir,
		SecretsDir:    secretsDir,
		StackTag:      stackTag,
		LabDockerfile: labDockerfile == nil,
	}); err != nil {
		return nil, err
	}
	return rendered.Bytes(), nil
}

// collectUpgradeValues prompts only for config a new release added.
//
// Re-asking for every value an operator already set would be a good way to
// have them paste the wrong one into a lab that was working.
func collectUpgradeValues(cmd *cobra.Command, plan *installer.UpgradePlan, deployDir string) (map[string]installer.Values, error) {
	needed, err := plan.NewConfig(deployDir)
	if err != nil {
		return nil, err
	}
	values := map[string]installer.Values{}
	if len(needed) == 0 {
		return values, nil
	}

	errOut := cmd.ErrOrStderr()
	for _, e := range plan.Entries {
		name := e.Plan.Manifest.Name
		wanted := needed[name]
		if len(wanted) == 0 {
			continue
		}
		fmt.Fprintf(errOut, "\n%s at %s needs configuration it did not before:\n", name, plan.ToTag)

		v := installer.Values{Config: map[string]string{}}
		for _, c := range e.Plan.Manifest.Config {
			if !contains(wanted, c.Env) {
				continue
			}
			if c.Help != "" {
				fmt.Fprintf(errOut, "  %s\n", c.Help)
			}
			answer, err := prompt.Line(c.Prompt, c.Default)
			if err != nil {
				return nil, err
			}
			v.Config[c.Env] = answer
		}
		values[name] = v
	}
	return values, nil
}

// warnMissingSecrets reports credentials a new release expects that are not on
// disk. Not an error — the upgrade itself succeeded — but the broker will fail
// on them at runtime, which is exactly the kind of delayed failure worth
// naming here rather than discovering in a container log.
func warnMissingSecrets(cmd *cobra.Command, plan *installer.UpgradePlan, secretsDir string) {
	for _, e := range plan.Entries {
		for _, s := range e.Plan.Manifest.Secrets {
			if s.Optional {
				continue
			}
			if _, err := os.Stat(filepath.Join(secretsDir, s.File)); err == nil {
				continue
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: %s expects %s in %s, and it is not there. The broker will fail on\n"+
					"         it at runtime. Store it with `sal secrets set %s`.\n",
				e.Plan.Manifest.Name, s.File, secretsDir, e.Plan.Manifest.Name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

