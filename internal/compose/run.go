package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	return append([]string{"compose", "-f", r.File}, rest...)
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

func dirOf(file string) string {
	if i := strings.LastIndexByte(file, '/'); i > 0 {
		return file[:i]
	}
	return "."
}
