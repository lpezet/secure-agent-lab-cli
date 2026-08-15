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
		Use:   "down",
		Short: "Stop labs across this machine",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented,
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every running lab on this machine")
	return cmd
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
	fmt.Fprintf(w, "\nEach running lab mounts the whole secrets directory into its broker, and hands\n"+
		"the agent only what its installed providers expose. Stop one with `sal down`\n"+
		"in its project.\n")
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
