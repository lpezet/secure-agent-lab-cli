package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/compose"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

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
		Short: "List every lab on this machine, and which of them are running",
		Long: "Answers \"what is currently running with my credentials attached?\", which makes\n" +
			"this a control rather than a convenience.\n\n" +
			"One stack per project is deliberate — sharing one would put two projects behind\n" +
			"a single proxy, a single audit trail and a single set of injected credentials,\n" +
			"with an agent working on one project holding credentials scoped for another.\n" +
			"The cost of that choice is six containers per project, and a forgotten lab is\n" +
			"not idle: it is a live credential-injecting proxy with the secrets directory\n" +
			"mounted.\n\n" +
			"So this reports every deployment, running or not, and checks each one's project\n" +
			"still exists and still points back at it. A lab running for a project that was\n" +
			"deleted is the exact thing being looked for.\n\n" +
			"It reads the filesystem and asks Docker once. Nothing here needs the network,\n" +
			"and no manifest is fetched: what a lab installed is recorded in the lab.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLabsList(cmd)
		},
	}
}

func newLabsDownCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "down [NAME...]",
		Short: "Stop labs anywhere on this machine, by name",
		Long: "The other half of `sal labs list`. That command finds a lab running for a\n" +
			"project that was deleted; this is what stops it — `sal down` cannot, because\n" +
			"it needs a project to find the lab from, and that is exactly what is missing.\n\n" +
			"Takes lab names, spelled exactly as `sal labs list` prints them, or --all.\n" +
			"Never both, and never neither: a bare `sal labs down` that stopped everything\n" +
			"would be a machine-wide action nobody typed.\n\n" +
			"There is deliberately no --volumes here, though `sal down` has one. That flag\n" +
			"deletes a lab's audit trail — the record of everything the agent did — and\n" +
			"across a whole machine that is not an operation with a safe shape. Deleting\n" +
			"one trail is a decision; deleting every trail is a decision made once and\n" +
			"applied to things the operator was not thinking about. Run `sal down --volumes`\n" +
			"per project, which makes you visit each one.\n\n" +
			"Stopping is reversible, so this does not prompt. It does print what it is\n" +
			"about to stop, and which of those were live.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLabsDown(cmd, args, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every lab on this machine that Docker knows about")
	return cmd
}

func runLabsDown(cmd *cobra.Command, names []string, all bool) error {
	errOut := cmd.ErrOrStderr()

	switch {
	case all && len(names) > 0:
		return errors.New("--all already names every lab; pass names or --all, not both")
	case !all && len(names) == 0:
		return errors.New("name a lab to stop, or pass --all; `sal labs list` prints the names.\n" +
			"To stop the lab for the project you are in, `sal down` is the shorter way")
	}

	labs, err := lab.All()
	if err != nil {
		return err
	}

	// Unlike `labs list`, an unreachable Docker is fatal here. That command
	// still has an inventory to report from the filesystem; this one has
	// nothing to do without a daemon, and asking once beats the same exec
	// failure repeated per lab.
	state, err := dockerState(cmd.Context())
	if err != nil {
		return err
	}

	targets, err := selectLabs(labs, names, all, state)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintf(errOut, "nothing to stop: Docker knows of no containers for any lab on this machine\n")
		reportUnknownProjects(errOut, rootOf(labs), state, namesOf(labs))
		return nil
	}

	// Each lab is stopped independently, and one that fails does not stop the
	// rest. This is the opposite of `sal upgrade`, where a partial run leaves
	// half a deployment on each of two releases — here a partial run just
	// leaves fewer labs running, and refusing to continue would leave MORE of
	// them up, which is the opposite of what was asked for.
	var failed []string
	stopped, wereRunning := 0, 0
	for _, l := range targets {
		p, known := state[l.Name]
		fmt.Fprintf(errOut, "\nstopping %s (%s)\n", l.Name, statusOf(p, known))

		r := &compose.Runner{File: l.ComposeFile(), Stdout: cmd.OutOrStdout(), Stderr: errOut}
		if err := r.Run(cmd.Context(), "down"); err != nil {
			fmt.Fprintf(errOut, "%s: %v\n", l.Name, err)
			failed = append(failed, l.Name)
			continue
		}
		stopped++
		// Counted only on success, and the distinction is the point of the
		// number: it says how many live credential paths were closed. A lab
		// that refused to stop is still running, and counting it here would
		// report the opposite of what happened.
		if p.Running() {
			wereRunning++
		}
	}

	fmt.Fprintf(errOut, "\nstopped %s", plural(stopped, "lab"))
	if wereRunning > 0 {
		// The number that matters: each of these was a live
		// credential-injecting proxy with the secrets directory mounted.
		fmt.Fprintf(errOut, ", %d of which %s running", wereRunning, was(wereRunning))
	}
	fmt.Fprintln(errOut, ".")

	if len(failed) > 0 {
		return fmt.Errorf("%s would not stop: %s", plural(len(failed), "lab"), strings.Join(failed, ", "))
	}
	return nil
}

// selectLabs turns names or --all into the deployments to act on.
//
// A name must be spelled exactly, for the same reason a credential must be:
// two projects called `api` are precisely what the hash suffix exists to keep
// apart, so prefix matching would be ambiguous exactly where it matters. The
// names are copyable out of `sal labs list`.
func selectLabs(labs []*lab.Lab, names []string, all bool, state map[string]compose.Project) ([]*lab.Lab, error) {
	byName := make(map[string]*lab.Lab, len(labs))
	for _, l := range labs {
		byName[l.Name] = l
	}

	if all {
		// Every lab Docker has containers for, running or merely present —
		// `down` removes both, and a lab that exited still has container
		// objects to clear. Labs Docker has never heard of are skipped because
		// there is genuinely nothing to do for them.
		var targets []*lab.Lab
		for _, l := range labs {
			if _, known := state[l.Name]; known {
				targets = append(targets, l)
			}
		}
		return targets, nil
	}

	var targets []*lab.Lab
	for _, name := range names {
		l, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("no lab named %q on this machine; `sal labs list` prints the names", name)
		}
		if !l.Exists() {
			// Nothing to drive compose with. `sal labs list` reports this
			// directory as not a deployment, and if containers ARE up under
			// that name it says so there too.
			return nil, fmt.Errorf("lab %q has no compose file at %s, so there is nothing to stop it with", name, l.Dir)
		}
		targets = append(targets, l)
	}
	return targets, nil
}

func statusOf(p compose.Project, known bool) string {
	if !known {
		return "not started"
	}
	return p.Status
}

func namesOf(labs []*lab.Lab) map[string]bool {
	known := make(map[string]bool, len(labs))
	for _, l := range labs {
		known[l.Name] = true
	}
	return known
}

func rootOf(labs []*lab.Lab) string {
	if len(labs) > 0 {
		return filepath.Dir(labs[0].Dir)
	}
	root, err := config.LabsDir()
	if err != nil {
		return ""
	}
	return root
}

func was(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func runLabsList(cmd *cobra.Command) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	root, err := config.LabsDir()
	if err != nil {
		return err
	}
	labs, err := lab.All()
	if err != nil {
		return err
	}

	// Docker is asked once, and a failure to reach it does not fail the
	// command: which labs EXIST is a fact about the filesystem and is worth
	// reporting on a machine whose daemon is down. The status column says
	// "unknown" rather than implying nothing is running.
	state, dockerErr := dockerState(cmd.Context())

	if len(labs) == 0 {
		fmt.Fprintf(errOut, "no labs on this machine (%s is empty); run `sal init` in a project to create one\n", root)
		reportUnknownProjects(errOut, root, state, nil)
		return nil
	}

	fmt.Fprintf(out, "%s\n\n", root)

	running := 0
	known := make(map[string]bool, len(labs))
	for _, l := range labs {
		known[l.Name] = true

		status, up := "unknown", false
		if state != nil {
			status = "not started"
			if p, ok := state[l.Name]; ok {
				status, up = p.Status, p.Running()
				if up {
					running++
				}
			}
		}
		fmt.Fprintf(out, "%s\n", l.Name)
		fmt.Fprintf(out, "  %-9s %s\n", "status", status)

		rec, recErr := deployment.Load(l.Dir)
		switch {
		case errors.Is(recErr, deployment.ErrNoRecord) && !l.Exists():
			// Not a deployment at all: a directory under labs/ that sal did
			// not finish creating, or did not finish deleting.
			note := "no compose file and no install record — not a deployment"
			if up {
				// Same finding as reportUnknownProjects, reached from the other
				// side: containers up, and nothing on disk describing them. It
				// arrives here rather than there because the directory still
				// exists, so the name is accounted for.
				note += ", yet its containers are up"
			}
			fmt.Fprintf(out, "  %-9s %s\n", "note", note)
		case recErr != nil:
			fmt.Fprintf(out, "  %-9s %v\n", "record", recErr)
		default:
			fmt.Fprintf(out, "  %-9s %s\n", "project", describeProject(l, rec))
			fmt.Fprintf(out, "  %-9s %s\n", "stack", describeStack(rec))
			fmt.Fprintf(out, "  %-9s %s\n", "providers", describeProviders(rec))
		}
		fmt.Fprintln(out)
	}

	summarise(errOut, labs, running, state, dockerErr)
	reportUnknownProjects(errOut, root, state, known)
	return nil
}

// dockerState maps compose project name to what Docker says about it.
//
// A nil map means the question could not be asked at all, which is different
// from an empty one — the first is "unknown", the second is "nothing is
// running". Collapsing them would let a machine with no docker report every
// lab as stopped, which is the reassuring answer and the wrong one.
func dockerState(ctx context.Context) (map[string]compose.Project, error) {
	if err := compose.Available(ctx); err != nil {
		return nil, err
	}
	projects, err := compose.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]compose.Project, len(projects))
	for _, p := range projects {
		byName[p.Name] = p
	}
	return byName, nil
}

// describeProject reports the recorded project directory AND whether it is
// still true, because the record is a claim made when the lab was created.
//
// Both failure modes here describe the same thing from different sides: a
// deployment nothing is using any more. It is worth naming precisely, since a
// lab whose project is gone is the one most likely to be running unnoticed.
func describeProject(l *lab.Lab, rec *deployment.Record) string {
	if rec.ProjectDir == "" {
		// Written by a sal that predates the field. Nothing is wrong with the
		// lab; the reverse lookup simply was not recorded, and the next
		// upgrade writes it.
		return "unrecorded — this lab predates sal recording it; `sal upgrade` writes it"
	}

	if _, err := os.Stat(rec.ProjectDir); errors.Is(err, fs.ErrNotExist) {
		return rec.ProjectDir + "  ← gone; nothing is using this lab"
	}

	// Deliberately not lab.Find: that walks UP, so a deleted pointer would be
	// answered by an ancestor project's and the check would pass on a lab
	// nothing points at.
	p, err := lab.PointerAt(rec.ProjectDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return rec.ProjectDir + "  ← the project has no .sal/lab.json; nothing is using this lab"
	case err != nil:
		return fmt.Sprintf("%s  ← %v", rec.ProjectDir, err)
	case p.Name != l.Name:
		return fmt.Sprintf("%s  ← that project now uses lab %q", rec.ProjectDir, p.Name)
	}
	return rec.ProjectDir
}

func describeStack(rec *deployment.Record) string {
	tag := rec.StackTag
	if tag == "" {
		tag = "unrecorded"
	}
	if rec.StackCommit != "" {
		return fmt.Sprintf("%s (%s)", tag, shortCommit(rec.StackCommit))
	}
	return tag
}

// describeProviders names the bank entries installed, which is as far as this
// command goes into credentials on purpose.
//
// Naming the individual credential files would mean fetching every manifest,
// putting a network round trip behind an inventory. `sal secrets list` is the
// command that resolves them, and it already does.
func describeProviders(rec *deployment.Record) string {
	names := rec.Names()
	if len(names) == 0 {
		return "none — no credential path installed"
	}
	return strings.Join(names, ", ")
}

// summarise puts the counts and the standing warning on stderr, so the
// inventory on stdout stays greppable.
func summarise(w io.Writer, labs []*lab.Lab, running int, state map[string]compose.Project, dockerErr error) {
	fmt.Fprintf(w, "%s on this machine", plural(len(labs), "lab"))
	if state == nil {
		fmt.Fprintf(w, ". Docker could not be asked which are running:\n  %v\n", dockerErr)
		return
	}
	fmt.Fprintf(w, ", %d running.\n", running)

	if running == 0 {
		return
	}
	// Stated only when it is true of something, and stated precisely: the
	// broker mounts the whole secrets directory, and what reaches the agent is
	// narrower than that — only the routes its installed providers expose.
	// `sal labs down` and not only `sal down`, because the labs most worth
	// stopping are the ones this command just reported as having no project
	// left — and `sal down` needs a project to find the lab from.
	fmt.Fprintf(w, "\nEach running lab mounts the whole secrets directory into its broker, and hands\n"+
		"the agent only what its installed providers expose. Stop one with\n"+
		"`sal labs down <name>`, or `sal down` from inside its project.\n")
}

// reportUnknownProjects names compose projects running out of the labs
// directory that no lab there accounts for.
//
// This is the loudest thing this command can find. The containers are up, the
// secrets directory is mounted into them, and the deployment that describes
// what they do has been deleted — so nothing on disk can say which providers
// that broker serves, and `sal` has no lab to stop.
func reportUnknownProjects(w io.Writer, root string, state map[string]compose.Project, known map[string]bool) {
	var orphans []compose.Project
	for _, p := range state {
		if known[p.Name] || !underDir(p.ConfigFiles, root) || !p.Running() {
			continue
		}
		orphans = append(orphans, p)
	}
	if len(orphans) == 0 {
		return
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })

	fmt.Fprintf(w, "\nwarning: %s running from %s with no deployment there:\n",
		plural(len(orphans), "compose project"), root)
	for _, p := range orphans {
		fmt.Fprintf(w, "  %s  (%s)\n", p.Name, p.ConfigFiles)
	}
	fmt.Fprintf(w, "Those containers are up with the secrets directory mounted, and the files that\n"+
		"would say which providers each broker serves are gone. Stop one with\n"+
		"`docker compose -p <name> down`.\n")
}

// underDir reports whether any of docker's config-file paths sits under dir.
// ConfigFiles is comma-separated, because compose accepts more than one file.
func underDir(configFiles, dir string) bool {
	for _, f := range strings.Split(configFiles, ",") {
		rel, err := filepath.Rel(dir, strings.TrimSpace(f))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
