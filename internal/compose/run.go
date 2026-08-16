package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Runner drives `docker compose` against one deployment.
//
// It shells out to the docker CLI rather than linking the Docker SDK or
// compose-go. Two reasons, and neither is laziness: the compose file's
// semantics are enormous and the CLI is the stable contract over them, and
// shelling out means sal never needs the Docker socket itself — so nothing
// here is a step away from root on the host.
type Runner struct {
	// File is the compose file. Its directory is the working directory, which
	// is what makes ./broker and friends resolve inside the deployment.
	File string

	Stdout, Stderr io.Writer
	Stdin          io.Reader

	// Project is the compose project name, passed on every invocation.
	//
	// The stack's template hardcodes `name: secure-agent-lab`, which is right
	// for a deployment somebody copies by hand and wrong for a machine running
	// one lab per project — every lab would be the same compose project, and
	// `sal labs list` could not tell them apart because there would be nothing
	// to tell apart. The flag wins over the file, and over COMPOSE_PROJECT_NAME
	// in .env, which sal also writes so a hand-run compose agrees.
	Project string

	// Profiles are the compose profiles to enable, passed explicitly on every
	// invocation rather than left to COMPOSE_PROFILES in the deployment's
	// .env.
	//
	// Compose reads that variable itself, and the file carries it so a
	// hand-run `docker compose up` behaves the same way sal does. But a
	// feature being ON is what keeps the audit trail served, and "sal's
	// behaviour depends on compose reading a file the way I believe it does"
	// is not a good thing to be wrong about. Passing them makes sal's own path
	// independent of that belief; the file makes everyone else's path agree
	// with it.
	Profiles []string
}

// ErrNoDocker means the docker CLI or its compose plugin is missing.
var ErrNoDocker = errors.New("docker with the compose plugin is required")

// Available reports whether docker and `docker compose` can be run at all.
// Checked before anything else so a missing prerequisite is one clear line
// rather than a wall of exec output.
func Available(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("%w: docker is not on PATH", ErrNoDocker)
	}
	if err := exec.CommandContext(ctx, "docker", "compose", "version").Run(); err != nil {
		return fmt.Errorf("%w: `docker compose version` failed, so the compose plugin is missing or the daemon is unreachable", ErrNoDocker)
	}
	return nil
}

func (r *Runner) args(rest ...string) []string {
	args := []string{"compose"}
	if r.Project != "" {
		args = append(args, "-p", r.Project)
	}
	for _, p := range r.Profiles {
		args = append(args, "--profile", p)
	}
	args = append(args, "-f", r.File)
	return append(args, rest...)
}

// Run streams a compose command's output to the caller's writers.
func (r *Runner) Run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", r.args(args...)...)
	cmd.Dir = dirOf(r.File)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = r.Stdout, r.Stderr, r.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Output captures stdout instead of streaming it.
func (r *Runner) Output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", r.args(args...)...)
	cmd.Dir = dirOf(r.File)
	cmd.Stderr = r.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker compose %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ObserverURL asks Docker which host port it assigned.
//
// sal never picks the port — the compose file publishes 127.0.0.1::9000 and
// this reads back the answer, which is what makes a collision between two labs
// impossible rather than something to track. An empty result means the service
// is not running.
func (r *Runner) ObserverURL(ctx context.Context) (string, error) {
	hostPort, err := r.Output(ctx, "port", "observer", "9000")
	if err != nil {
		return "", err
	}
	if hostPort == "" {
		return "", nil
	}
	// docker prints host:port, and the host half is the loopback address the
	// compose file bound to.
	return "http://" + strings.TrimSpace(hostPort), nil
}

// Project is one compose project as `docker compose ls` reports it.
type Project struct {
	Name        string
	Status      string
	ConfigFiles string
}

// Running reports whether any container in the project is up.
//
// Status is docker's own summary and looks like "running(6)", or
// "running(3), exited(3)" for a project that partly came up. A substring match
// is deliberate over parsing the counts: a lab with one container running is
// as much a live credential path as one with six, and no other container state
// docker reports — created, restarting, paused, exited, dead, removing —
// contains this word.
func (p Project) Running() bool { return strings.Contains(p.Status, "running") }

// List asks Docker which compose projects it knows about, running or not.
//
// One call for the whole machine rather than one per lab: the answer is a
// property of the daemon, not of any deployment, which is also why it is a
// package function and not a Runner method.
func List(ctx context.Context) ([]Project, error) {
	cmd := exec.CommandContext(ctx, "docker", "compose", "ls", "--all", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose ls: %w", err)
	}

	var projects []Project
	if err := json.Unmarshal(out, &projects); err != nil {
		// The caller reports labs from the filesystem and marks their state
		// unknown. Refusing outright would make an inventory of what exists —
		// which is true regardless of what docker says — depend on a format
		// this repo does not own.
		return nil, fmt.Errorf("docker compose ls returned output this sal cannot read: %w", err)
	}
	return projects, nil
}

// DefaultProfiles are the profiles a new deployment starts with enabled.
//
// This RESTATES what the template declares, and the two must move together: a
// profile added there and not here ships disabled, which for anything shaped
// like the observer means a lab quietly serving no audit trail. Same shape as
// the load-band ranges restated in internal/installer — a constant that lives
// somewhere else and is written down here because it has to be known before
// anything can ask Docker about it. TestEveryProfileIsAService keeps the two
// honest.
var DefaultProfiles = []string{"observer"}

// DefinedProfiles returns every profile this deployment's compose file
// declares, which is the list of features it has.
//
// Read from the file rather than from a list in sal: a feature is data, in the
// same way a provider is. The template decides what is optional and this
// reports it, so a profile added to the template needs no code here.
func (r *Runner) DefinedProfiles(ctx context.Context) ([]string, error) {
	out, err := r.Output(ctx, "config", "--profiles")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ContainerNames asks `docker ps` which running containers match the given
// label filters, each spelled `key=value`.
//
// Not a compose call, and deliberately so: it is how sal finds a container
// that is NOT part of the deployment — a dev container VS Code started for
// this project, which carries the extension's own labels and belongs to no
// compose project of ours. Reading it through `docker compose` would only find
// the ones already known to be the lab's, which is the opposite of the
// question being asked.
func ContainerNames(ctx context.Context, labels ...string) ([]string, error) {
	args := []string{"ps", "--format", "{{.Names}}"}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

func dirOf(file string) string {
	if i := strings.LastIndexByte(file, '/'); i > 0 {
		return file[:i]
	}
	return "."
}
