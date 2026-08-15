package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/compose"
	"github.com/lpezet/secure-agent-lab-cli/internal/envfile"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

// profilesVar is the variable compose itself reads to decide which profiles
// are on. sal writes the same one rather than inventing its own key, so a
// deployment driven by hand behaves the way sal says it does.
const profilesVar = "COMPOSE_PROFILES"

// envFileName is the deployment's own environment, read by broker and proxy.
// labEnvFileName is the lab container's, and they are two files rather than
// one because the lab must never receive the broker's environment.
const (
	envFileName    = ".env"
	labEnvFileName = "lab.env"
)

// newFeaturesCmd builds the `sal features` group.
//
// Enable, disable and list are the same operation for every feature, so they
// live here rather than being copied into each feature's own group. If each
// feature owned a copy there would be no single place to answer "what is on?",
// which is the question that matters on a security tool.
//
// This mirrors gcloud, which does both: `gcloud services enable NAME` for
// lifecycle and `gcloud run deploy` for a service's own actions — and notably
// `gcloud services disable NAME`, never `gcloud run disable`.
//
// A feature is a compose PROFILE, and the service it turns on has the same
// name. That equivalence is the whole implementation: nothing here knows what
// the observer is, only that the compose file declares a profile called one
// thing and a service called the same thing. compose.TestEveryProfileIsAService
// is what keeps that true of the template.
func newFeaturesCmd() *cobra.Command {
	group := newGroup("features", "Turn optional parts of the stack on and off")
	group.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List features and whether each is enabled",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runFeaturesList(cmd)
			},
		},
		&cobra.Command{
			Use:   "enable NAME",
			Short: "Enable a feature in this lab",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFeatureSet(cmd, args[0], true)
			},
		},
		&cobra.Command{
			Use:   "disable NAME",
			Short: "Disable a feature in this lab",
			Long: "Stops the feature's service and removes its container, then records it off\n" +
				"so it does not come back on the next `sal up`.\n\n" +
				"In that order, deliberately. Recording first and failing to stop would leave\n" +
				"the record saying a feature is off while it is running; for the observer that\n" +
				"is only untidy, but the reverse of it — believing a feature is ON when it is\n" +
				"not — is how someone ends up trusting an audit trail that nothing is writing.\n" +
				"`sal features list` reports both, so the two can be seen to disagree.\n\n" +
				"Volumes are untouched: disabling the observer does not delete the trail it\n" +
				"was serving.",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFeatureSet(cmd, args[0], false)
			},
		},
	)
	return group
}

// feature is one profile and what is known about it.
type feature struct {
	Name string

	// Configured is what .env says: whether the next `sal up` starts it.
	Configured bool

	// Running is what Docker says right now.
	Running bool
}

func runFeaturesList(cmd *cobra.Command) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, r, err := runnerFor(cmd)
	if err != nil {
		return err
	}
	features, labUp, err := readFeatures(cmd, l, r)
	if err != nil {
		return err
	}
	if len(features) == 0 {
		fmt.Fprintf(errOut, "lab %s declares no features.\n"+
			"A lab created before sal had any keeps its original compose file until\n"+
			"`sal upgrade` re-renders it.\n", l.Name)
		return nil
	}

	for _, f := range features {
		fmt.Fprintf(out, "%-12s %-8s %s\n", f.Name, onOff(f.Configured), runningWord(f, labUp))
	}

	// A feature the record says is on and Docker says is not is the finding
	// worth interrupting for: it is how someone comes to trust an audit trail
	// that nothing is writing.
	for _, f := range features {
		if labUp && f.Configured && !f.Running {
			fmt.Fprintf(errOut, "\nwarning: %s is enabled but not running, in a lab that is up.\n"+
				"         Whatever it does is not being done. `sal up` starts it.\n", f.Name)
		}
		if !f.Configured && f.Running {
			fmt.Fprintf(errOut, "\nnote: %s is running although it is disabled here. It will not come\n"+
				"      back after the next `sal down`.\n", f.Name)
		}
	}
	return nil
}

func runFeatureSet(cmd *cobra.Command, name string, enable bool) error {
	errOut := cmd.ErrOrStderr()

	l, r, err := runnerFor(cmd)
	if err != nil {
		return err
	}
	features, labUp, err := readFeatures(cmd, l, r)
	if err != nil {
		return err
	}

	current, ok := find(features, name)
	if !ok {
		return fmt.Errorf("lab %q has no feature %q.\n%s", l.Name, name, featureMenu(features))
	}
	if current.Configured == enable {
		fmt.Fprintf(errOut, "%s is already %s; nothing to do\n", name, onOff(enable))
		return nil
	}

	envPath := filepath.Join(l.Dir, envFileName)

	if enable {
		// Recorded first, then started: an enable that records and fails to
		// start leaves `features list` reporting the disagreement, where
		// starting without recording would come undone at the next `sal up`
		// with nothing to show why.
		if err := writeProfiles(envPath, withFeature(features, name, true)); err != nil {
			return err
		}
		if !labUp {
			fmt.Fprintf(errOut, "%s is enabled and starts with the lab. `sal up` when you want it\n", name)
			return nil
		}
		// The service and the profile share a name, so enabling one names the
		// other. Its own profile is passed explicitly, because .env has only
		// just learned about it.
		up := *r
		up.Profiles = withProfile(r.Profiles, name)
		if err := up.Run(cmd.Context(), "up", "-d", name); err != nil {
			return err
		}
		fmt.Fprintf(errOut, "%s enabled and started\n", name)
		return nil
	}

	// Stopped BEFORE the record changes, and with the profile named
	// explicitly: once .env no longer enables it, compose does not consider
	// the service to exist and cannot be asked to remove it.
	if current.Running {
		down := *r
		down.Profiles = withProfile(r.Profiles, name)
		// --stop so a running container is stopped first, and no --volumes
		// anywhere near it: disabling the observer must not delete the trail
		// it was serving.
		if err := down.Run(cmd.Context(), "rm", "--stop", "--force", name); err != nil {
			return err
		}
	}
	if err := writeProfiles(envPath, withFeature(features, name, false)); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "%s disabled. Its volumes are untouched, so nothing it wrote is lost\n", name)
	return nil
}

// readFeatures gathers what the compose file declares, what .env enables, and
// what Docker is actually running. It also reports whether the lab is up at
// all, which is what makes "enabled but not running" a finding rather than the
// ordinary state of a stopped lab.
func readFeatures(cmd *cobra.Command, l *lab.Lab, r *compose.Runner) ([]feature, bool, error) {
	defined, err := r.DefinedProfiles(cmd.Context())
	if err != nil {
		return nil, false, fmt.Errorf("cannot read the features %s declares: %w", l.Name, err)
	}

	enabled, err := enabledProfiles(l.Dir, defined)
	if err != nil {
		return nil, false, err
	}
	on := make(map[string]bool, len(enabled))
	for _, e := range enabled {
		on[e] = true
	}

	labUp, err := serviceRunning(cmd, r)
	if err != nil {
		return nil, false, err
	}

	features := make([]feature, 0, len(defined))
	for _, name := range defined {
		running, err := profileServiceRunning(cmd, r, name)
		if err != nil {
			return nil, false, err
		}
		features = append(features, feature{Name: name, Configured: on[name], Running: running})
	}
	return features, labUp, nil
}

// enabledProfiles reads COMPOSE_PROFILES, treating an ABSENT variable as every
// feature enabled.
//
// Absence has to mean on. The alternative — absence means off — turns a
// deployment whose .env lost a line, or one created before features existed,
// into a lab that quietly serves no audit trail. A feature that fails on is
// visible; a feature that fails off is not.
func enabledProfiles(deployDir string, defined []string) ([]string, error) {
	values, err := envfile.Read(filepath.Join(deployDir, envFileName))
	if err != nil {
		return nil, err
	}
	raw, present := values[profilesVar]
	if !present {
		return defined, nil
	}

	var enabled []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			enabled = append(enabled, name)
		}
	}
	return enabled, nil
}

func writeProfiles(envPath string, names []string) error {
	sort.Strings(names)
	return envfile.Upsert(envPath, map[string]string{profilesVar: strings.Join(names, ",")})
}

// withFeature returns the enabled set with one name added or removed.
func withFeature(features []feature, name string, enable bool) []string {
	var names []string
	for _, f := range features {
		switch {
		case f.Name == name && enable:
			names = append(names, f.Name)
		case f.Name == name:
			// dropped
		case f.Configured:
			names = append(names, f.Name)
		}
	}
	return names
}

// profileServiceRunning reports whether a feature's own service has a
// container. The profile is named explicitly so the answer does not depend on
// whether .env currently enables it — the question is about Docker, not about
// configuration.
func profileServiceRunning(cmd *cobra.Command, r *compose.Runner, name string) (bool, error) {
	q := *r
	q.Profiles = withProfile(r.Profiles, name)
	q.Stderr = io.Discard

	out, err := q.Output(cmd.Context(), "ps", "--quiet", name)
	if err != nil {
		// A service that compose cannot resolve is not running, and saying so
		// is better than refusing to list anything.
		return false, nil
	}
	return strings.TrimSpace(out) != "", nil
}

// withProfile adds a profile to a set without repeating one already there.
// Compose would accept the repeat; a log that shows it twice makes a test
// about which profiles a call carried read as though something were wrong.
func withProfile(profiles []string, name string) []string {
	out := append([]string{}, profiles...)
	for _, p := range profiles {
		if p == name {
			return out
		}
	}
	return append(out, name)
}

func find(features []feature, name string) (feature, bool) {
	for _, f := range features {
		if f.Name == name {
			return f, true
		}
	}
	return feature{}, false
}

func featureMenu(features []feature) string {
	if len(features) == 0 {
		return "This lab declares none."
	}
	var b strings.Builder
	b.WriteString("It has:\n")
	for _, f := range features {
		fmt.Fprintf(&b, "  %-12s %s\n", f.Name, onOff(f.Configured))
	}
	return strings.TrimRight(b.String(), "\n")
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// runningWord describes Docker's answer, and says nothing at all when the lab
// itself is down: "not running" would be true of every service and would read
// as a finding about the feature.
func runningWord(f feature, labUp bool) string {
	switch {
	case f.Running:
		return "running"
	case !labUp:
		return "lab is not running"
	default:
		return "not running"
	}
}

// configuredProfiles is what every command passes to compose.
//
// An absent COMPOSE_PROFILES means every feature this build knows about,
// rather than none: a lab created before features existed, or one whose .env
// lost a line, must not come up quietly serving no audit trail. That is why
// the fallback is compose.DefaultProfiles and not an empty list — and why it
// is a constant here rather than a question for Docker, since this runs before
// there is a runner to ask with.
func configuredProfiles(deployDir string) ([]string, error) {
	values, err := envfile.Read(filepath.Join(deployDir, envFileName))
	if err != nil {
		return nil, err
	}
	raw, present := values[profilesVar]
	if !present {
		return compose.DefaultProfiles, nil
	}

	var enabled []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			enabled = append(enabled, name)
		}
	}
	return enabled, nil
}

// backfillProfiles writes the variable when a deployment has none, and leaves
// an existing value alone — including an empty one, which is somebody having
// turned everything off on purpose.
func backfillProfiles(deployDir string) error {
	envPath := filepath.Join(deployDir, envFileName)
	values, err := envfile.Read(envPath)
	if err != nil {
		return err
	}
	if _, present := values[profilesVar]; present {
		return nil
	}
	return writeProfiles(envPath, compose.DefaultProfiles)
}

// featureDisabled reports whether a named feature is switched off, reading
// only .env — no Docker, because this answers a question asked on a path that
// has already failed to reach Docker for something else.
func featureDisabled(deployDir, name string) bool {
	values, err := envfile.Read(filepath.Join(deployDir, envFileName))
	if err != nil {
		return false
	}
	raw, present := values[profilesVar]
	if !present {
		return false
	}
	for _, p := range strings.Split(raw, ",") {
		if strings.TrimSpace(p) == name {
			return false
		}
	}
	return true
}

// reportFeatures says which features came up, after `sal up`.
//
// Printed every time rather than only when something is off. A feature being
// off is not an error and does not deserve a warning, but "the audit trail is
// not being served" is not a thing to find out by noticing an absence — and
// the cheapest guard against a lab quietly running without one is to say so
// where somebody is already looking.
func reportFeatures(cmd *cobra.Command, l *lab.Lab, r *compose.Runner) {
	features, _, err := readFeatures(cmd, l, r)
	if err != nil {
		return
	}
	for _, f := range features {
		fmt.Fprintf(cmd.OutOrStdout(), "feature  %-12s %s\n", f.Name, onOff(f.Configured))
	}
}
