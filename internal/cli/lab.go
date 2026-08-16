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

	// Refused before anything is created. Below this release the deployment
	// template either does not exist at this path or names specific bank
	// entries in the broker's environment — and `environment:` wins over
	// `env_file:`, so a lab built from it would read credential paths the
	// template chose rather than the ones its manifests declare.
	if !version.StackHasUsableTemplate(stackTag) {
		return fmt.Errorf("sal cannot create a lab at stack %s: the deployment template it installs "+
			"is only usable from v%s onwards.\nPin this lab to v%s or newer, or use an older sal",
			stackTag, version.TemplateFrom, version.TemplateFrom)
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
	// a directory the user cannot write. The path also goes into .env, because
	// the template takes it as AGENT_CREDS_DIR rather than assuming a location.
	secretsDir, err := secretsDirFor()
	if err != nil {
		return err
	}

	// No proxy addons are installed, and none can be: a lab sal creates is
	// always at a release that carries them in the proxy image, because the
	// template it is built from only exists at those releases. The predicate
	// that draws that line still matters for labs sal did NOT create — see
	// version.StackBakesAddons, which `sal drift` reads when it meets one.
	if err := createLabTree(labDir); err != nil {
		return err
	}
	var installed []string

	// The wiring comes from the stack at the pinned release, written verbatim.
	// The file names its own tag in every build: line, so what this lab builds
	// is fixed by which tag it was fetched at — and `sal upgrade` is the same
	// fetch at a new one.
	newLab := &lab.Lab{Name: name, Dir: labDir, ProjectDir: projectDir}
	if _, err := installTemplate(cmd, newLab, commit, everyTemplateFile()); err != nil {
		return err
	}

	// Both start empty and are filled by `sal providers add` from manifests.
	// Compose requires the files to exist, and keeping them separate is what
	// stops the lab container receiving the broker's environment.
	if err := ensureEnvFiles(labDir); err != nil {
		return err
	}
	// Written out rather than left to a default, so a `docker compose up` run
	// by hand starts the same services `sal up` does. sal treats an absent
	// value as every feature on, which is the safe direction for a lab that
	// predates features — but compose treats it as none of them, and a lab
	// serving no audit trail because nobody wrote a line down is not a
	// difference worth leaving on the table.
	if err := writeProfiles(filepath.Join(labDir, ".env"), compose.DefaultProfiles); err != nil {
		return err
	}
	// The values the template asks for by name. Written after the comment
	// header above, so the file reads as prose then settings.
	if err := writeWiringEnv(newLab, secretsDir); err != nil {
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
	fmt.Fprintf(out, "wiring   %s, fetched at %s\n", composeName, stackTag)
	// Said rather than left as an absence: a reader who knows the older shape
	// would otherwise wonder which control went missing.
	fmt.Fprintf(out, "addons   carried by the proxy image at %s, not vendored here\n", stackTag)

	warnAboutTheAllowlist(cmd, newLab)

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
	reportFeatures(cmd, l, r)
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
	profiles, err := configuredProfiles(l.Dir)
	if err != nil {
		return nil, nil, err
	}
	return l, &compose.Runner{
		File:     l.ComposeFile(),
		Project:  l.Name,
		Stdout:   cmd.OutOrStdout(),
		Stderr:   cmd.ErrOrStderr(),
		Profiles: profiles,
	}, nil
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
	// The same floor init has, for the same reason: an upgrade re-fetches the
	// deployment template, and below this release there is none sal can use.
	if !version.StackHasUsableTemplate(to) {
		return fmt.Errorf("sal cannot upgrade a lab to stack %s: the deployment template it installs "+
			"is only usable from v%s onwards", to, version.TemplateFrom)
	}

	toCommit := ""
	if local := stackDir(cmd); local == "" {
		toCommit, err = bank.DefaultSource().ResolveTag(cmd.Context(), to)
		if err != nil {
			return err
		}
	}

	// Same pin, and it still runs.
	//
	// This used to return here — "already at v1.12.0; nothing to do" — which is
	// the exact assumption this repo exists to refute: the pin is NOT what
	// determines the files. They are bind-mounted copies, so a lab can be
	// pinned to a release and not be running what that release ships, which is
	// the whole reason `sal drift` exists.
	//
	// It was also a promise broken in one hop. `sal drift` reports a tampered
	// addon and says "DRIFT and MISSING are what `sal upgrade` rewrites" — and
	// upgrade then declined, so the one command named as the remedy did
	// nothing about the finding. An agent editing a proxy addon is precisely
	// what drift is for, and the repair has to work. A remedy a lab can be too
	// up-to-date to receive is not a remedy.
	//
	// Deliberately OUTSIDE the resolution branch above. It lived inside it,
	// where --stack-dir skips it entirely — so the two paths did different
	// things and no test could reach the one that returned early, which is how
	// this survived. Both now take the same decision, and there is one place
	// where an early return could ever be re-added.
	//
	// Applying costs a fetch and some file writes; not work worth skipping.
	if to == rec.StackTag && (toCommit == "" || toCommit == rec.StackCommit) {
		fmt.Fprintf(errOut, "lab %s is already pinned to %s, so this reinstalls every file it ships\n"+
			"rather than moving the pin.\n", l.Name, to)
	}

	b, tree, err := openBank(cmd, toCommit)
	if err != nil {
		return err
	}
	defer tree.Close()

	// No addons directory, ever. An upgrade target is at or above the release
	// that carries them in the image — the floor below refuses anything
	// older — and an empty directory tells the planner exactly that, which
	// also makes every addon the lab currently vendors stale. Stale files are
	// deleted, which is the tidying the stack's own upgrade notes ask a human
	// to do by hand.
	plan, err := installer.BuildUpgradePlan(b, "", rec, to, toCommit)
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
		why := "not shipped at " + plan.ToTag
		if version.StackBakesAddons(plan.ToTag) {
			why = "carried by the proxy image at " + plan.ToTag
		}
		fmt.Fprintf(out, "DELETE   %s (%s)\n", a, why)
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

	// A lab created before features existed has no COMPOSE_PROFILES, and the
	// re-render below gives its compose file profiles for the first time. sal
	// would read the absent value as everything on; compose, run by hand,
	// would read it as nothing on. Writing it settles the disagreement in the
	// direction that keeps the audit trail served.
	// Both env files must EXIST before anything runs compose against this
	// deployment: the template names them with env_file:, and compose refuses
	// a file whose env_file is missing — for the whole project, not just the
	// service that reads it. A lab created before sal wrote lab.env would
	// otherwise upgrade successfully and then fail every later command.
	if err := ensureEnvFiles(l.Dir); err != nil {
		return err
	}
	if err := backfillProfiles(l.Dir); err != nil {
		return err
	}

	// And the values the template reads by name. A lab created before sal
	// fetched the template has none of them, and every default in that file is
	// wrong for a sal-managed deployment: ${WORKSPACE_DIR:-./workspace} would
	// mount an empty directory inside the deployment instead of the project,
	// and ${AGENT_CREDS_DIR:-$HOME/.config/agent-creds} would look for
	// credentials in the location sal deliberately does not use. Both fail
	// quietly — the lab comes up and does nothing useful.
	if err := writeWiringEnv(l, secretsDir); err != nil {
		return err
	}

	// Re-fetched last: if anything above failed, the compose file still
	// describes the release the files on disk came from.
	//
	// Only the compose file. The allowlist is the operator's egress policy and
	// lab/Dockerfile is the one image they build themselves — an upgrade that
	// rewrote either would throw away their work to apply a change to
	// something else.
	if _, err := installTemplate(cmd, l, toCommit, map[string]bool{composeName: true}); err != nil {
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
