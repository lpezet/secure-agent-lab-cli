// Package compose renders a deployment's compose file and drives docker
// compose against it.
//
// The template embedded here is a REFERENCE IMPLEMENTATION living temporarily
// on this side. It describes the stack's wiring, which is the boundary's
// business and not this CLI's — so a change to the stack's service graph
// currently needs a sal release, which is the coupling the two-repo split
// exists to prevent. The intended end state is that the stack repo carries
// this and sal fetches it at the pinned tag, the way it already fetches the
// bank. Keep it in one file and keep it dumb, so moving it is a copy.
//
// What must NOT drift into it is per-provider knowledge. Manifests declare
// their own secrets, config and lab_env, and those are written to .env and
// lab.env by the installer — never templated here. TestNoProviderNamesInOutput
// is the guard, because the AST invariant test only reads .go files and would
// never see a provider name in a template.
package compose

import (
	"embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed templates/compose.yaml.tmpl
var templates embed.FS

// StackRepo is the git source each service builds from.
//
// Builds rather than pulls because the stack publishes no images today. When
// it does, this becomes an image reference and `up` stops taking minutes on
// first run.
const StackRepo = "https://github.com/lpezet/secure-agent-lab.git"

// Data is everything the template needs. Every field is a fact about this
// deployment; none of it is about any provider.
type Data struct {
	// ProjectName doubles as the Docker Compose project name, which is what
	// scopes volumes — and therefore what gives each lab its own CA and its
	// own audit trail for free.
	ProjectName string

	// ProjectDir is the absolute path mounted at /workspace.
	ProjectDir string

	// SecretsDir is the absolute path mounted read-only at /secrets.
	SecretsDir string

	StackTag  string
	StackRepo string

	// LabDockerfile switches the lab service to a deployment-owned build.
	// Absent by default: a virgin lab runs the stack's own image, and agent
	// tooling is something added afterwards rather than assumed.
	LabDockerfile bool
}

// Render writes the compose file for a deployment.
func Render(w io.Writer, d Data) error {
	if d.StackRepo == "" {
		d.StackRepo = StackRepo
	}
	if err := d.validate(); err != nil {
		return err
	}

	t, err := template.New("compose.yaml.tmpl").
		Option("missingkey=error").
		ParseFS(templates, "templates/compose.yaml.tmpl")
	if err != nil {
		return err
	}
	return t.Execute(w, d)
}

func (d Data) validate() error {
	var missing []string
	for name, v := range map[string]string{
		"project name": d.ProjectName,
		"project dir":  d.ProjectDir,
		"secrets dir":  d.SecretsDir,
		"stack tag":    d.StackTag,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("cannot render compose: missing %s", strings.Join(missing, ", "))
	}

	// Both are interpolated into volume mounts, where a relative path would
	// silently resolve against the deployment directory instead of the one
	// intended — mounting the wrong tree at /workspace rather than failing.
	for name, p := range map[string]string{
		"project dir": d.ProjectDir,
		"secrets dir": d.SecretsDir,
	} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("cannot render compose: %s %q must be absolute", name, p)
		}
	}
	return nil
}
