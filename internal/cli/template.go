package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/envfile"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

// The deployment's wiring comes from the stack repo, fetched at the pinned
// tag and written VERBATIM.
//
// sal rendered its own compose file until stack 1.12.0. That was always
// temporary and CLAUDE.md said so: the service graph is the boundary's
// business, so a change to it needed a sal release — the coupling the two-repo
// split exists to prevent. Three things had to be true before the stack's own
// template could be used as-is, and each was a change over there:
//
//   - the observer's host port had to be assignable rather than fixed, or two
//     labs on one machine collide (stack #79);
//   - the observer had to sit behind a compose profile, or `sal features` had
//     nothing to switch (stack #80);
//   - the file had to name no provider, or every lab's compose would list
//     credential paths for entries it does not have — and `environment:` wins
//     over `env_file:`, so those would have overridden what a manifest
//     declared (stack #95, #96).
//
// What is left is parameterised entirely by .env and lab.env, both of which
// sal writes. So there is nothing to render, and `sal upgrade` is the same
// fetch at a new tag.
const (
	composeName    = lab.ComposeName
	allowlistName  = "allowlist"
	labDockerfile  = "lab/Dockerfile"
	projectNameVar = "COMPOSE_PROJECT_NAME"
)

// templateFiles are what a deployment takes from the template.
//
// Not the whole directory: workspace/ is where a hand-copied deployment puts
// the project it works on, and sal's project lives outside the deployment
// entirely — it is mounted in by path. README.md and .env.example describe the
// copy-it-yourself flow, which is not the flow a sal-managed lab is in.
var templateFiles = []string{composeName, allowlistName, labDockerfile}

// installTemplate writes the wiring into a deployment.
//
// overwrite says which files a caller is prepared to replace. On `init` that is
// all of them; on `upgrade` it is the compose file alone — the allowlist is the
// operator's egress policy and lab/Dockerfile is the one image they build
// themselves, so an upgrade that rewrote either would throw away work in order
// to apply a change to something else.
func installTemplate(cmd *cobra.Command, l *lab.Lab, commit string, overwrite map[string]bool) ([]string, error) {
	tree, err := bank.FetchTree(cmd.Context(), commit, bank.TemplateSubtree, bankOptions(cmd))
	if err != nil {
		return nil, fmt.Errorf("cannot obtain the deployment template, without which there is no lab to create: %w", err)
	}
	defer tree.Close()

	var written []string
	for _, rel := range templateFiles {
		src := filepath.Join(tree.Dir, filepath.FromSlash(rel))
		dst := filepath.Join(l.Dir, filepath.FromSlash(rel))

		body, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("the template at this release has no %s: %w", rel, err)
		}
		if _, err := os.Stat(dst); err == nil && !overwrite[rel] {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			return nil, err
		}
		written = append(written, rel)
	}
	return written, nil
}

// templateCompose returns the compose file this deployment's release ships, so
// `sal drift` can compare what is on disk against it.
func templateCompose(cmd *cobra.Command, commit string) ([]byte, error) {
	tree, err := bank.FetchTree(cmd.Context(), commit, bank.TemplateSubtree, bankOptions(cmd))
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	return os.ReadFile(filepath.Join(tree.Dir, composeName))
}

// writeWiringEnv sets the variables the template reads.
//
// Every one of them is a value the wiring asks for by name, and none of them
// is provider knowledge — that arrives separately, from each manifest, when
// `sal providers add` runs.
//
// COMPOSE_PROJECT_NAME is here for the same reason sal also passes -p on every
// invocation: the template hardcodes `name: secure-agent-lab`, which is right
// for the deployment someone copies by hand and wrong for a machine running
// one lab per project. The flag wins over this file and this file wins over
// the template, so sal's own path does not depend on compose reading .env —
// and a `docker compose` run by hand in the lab directory still gets the right
// project rather than colliding with every other lab.
func writeWiringEnv(l *lab.Lab, secretsDir string) error {
	return envfile.Upsert(filepath.Join(l.Dir, envFileName), map[string]string{
		projectNameVar:    l.Name,
		"WORKSPACE_DIR":   l.ProjectDir,
		"AGENT_CREDS_DIR": secretsDir,
		// Empty on purpose: the template writes 127.0.0.1:${OBSERVER_PORT-9000},
		// and an empty value leaves the host port for Docker to assign. A fixed
		// one would have two labs on a machine fighting over 9000.
		"OBSERVER_PORT": "",
	})
}

// warnAboutTheAllowlist says what a new lab can reach, which is nothing.
//
// The template ships the allowlist file present and empty, and present is what
// makes it enforcing — so a lab starts denying every destination. That is the
// right default and a surprising one, and finding it out from a failed request
// inside the container is a bad way to learn it.
func warnAboutTheAllowlist(cmd *cobra.Command, l *lab.Lab) {
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\nThe egress allowlist is enforcing and empty, so the lab can reach nothing yet.\n"+
			"Add the destinations your agent needs to %s — one per line, `domain [METHODS]`.\n"+
			"Deleting that file permits everything instead, with a warning at startup.\n",
		filepath.Join(l.Dir, allowlistName))
}

// secretsDirFor is config.SecretsDir with the error wrapped where it matters:
// the broker mounts this path, and a directory Docker creates root-owned
// instead is one no later `sal secrets set` can write to.
func secretsDirFor() (string, error) {
	dir, err := config.SecretsDir()
	if err != nil {
		return "", fmt.Errorf("cannot prepare the secrets directory the broker mounts: %w", err)
	}
	return dir, nil
}

// everyTemplateFile is the overwrite set `init` uses: a new deployment has no
// work in it to lose.
func everyTemplateFile() map[string]bool {
	all := make(map[string]bool, len(templateFiles))
	for _, f := range templateFiles {
		all[f] = true
	}
	return all
}

// ensureEnvFiles creates the two env files if they are not there, and leaves
// whatever is in them alone.
//
// Two files rather than one, and that is a boundary property: .env is the
// broker's and proxy's environment, lab.env is the lab container's, and the
// lab must never receive the broker's. The stack's template arrived at the
// same split independently — its lab service takes env_file: lab.env for the
// same reason.
//
// They must EXIST before anything runs compose here. The template names both
// with env_file:, and compose refuses a file whose env_file is missing for the
// whole project rather than for the service that reads it.
func ensureEnvFiles(labDir string) error {
	for name, comment := range map[string]string{
		envFileName: "Broker and proxy configuration, plus the values the deployment template\n" +
			"reads by name. Written by `sal init` and by `sal providers add` from each\n" +
			"provider's manifest.",
		labEnvFileName: "Environment for the lab container only, from each provider's lab_env.\n" +
			"Separate from .env so the lab never receives the broker's environment.",
	} {
		path := filepath.Join(labDir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := writeEnvFile(path, comment); err != nil {
			return err
		}
	}
	return nil
}
