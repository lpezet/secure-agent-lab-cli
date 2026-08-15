package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/envfile"
	"github.com/lpezet/secure-agent-lab-cli/internal/secrets"
)

// Values are what the operator supplied for a plan's prompts.
type Values struct {
	// Secrets maps a secret's env var to its value. An absent entry means the
	// secret was skipped — already present on disk, or optional and declined.
	// Values are held only long enough to be written.
	Secrets map[string][]byte

	// Config maps a config env var to its non-secret value.
	Config map[string]string
}

// Apply writes everything the plan decided, and returns the record entry.
//
// Order within this function matters less than the fact that BuildPlan already
// refused everything it was going to refuse: by the time Apply runs, the only
// remaining failures are I/O.
func (p *Plan) Apply(deployDir, secretsDir, stackTag string, v Values) (*deployment.Entry, error) {
	// Secrets first. They are the only thing here written outside the
	// deployment, and the only thing whose modes are load-bearing.
	for env, value := range v.Secrets {
		file, ok := p.SecretFiles[env]
		if !ok {
			return nil, fmt.Errorf("no secret named %s in this plan", env)
		}
		dst := filepath.Join(secretsDir, file)
		if err := os.WriteFile(dst, value, secrets.Perm); err != nil {
			return nil, err
		}
		// WriteFile does not chmod a file that already existed.
		if _, err := secrets.EnsureMode(dst); err != nil {
			return nil, err
		}
	}

	var written []string
	for _, f := range p.Files {
		body, err := os.ReadFile(f.Src)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(deployDir, f.Dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, body, f.Mode); err != nil {
			return nil, err
		}
		written = append(written, f.Dst)
	}
	sort.Strings(written)

	// .env carries the broker's and proxy's configuration: paths to secrets,
	// never their contents, plus non-secret config.
	env := map[string]string{}
	for k, v := range p.SecretEnv {
		env[k] = v
	}
	for k, v := range v.Config {
		env[k] = v
	}
	if len(env) > 0 {
		if err := envfile.Upsert(filepath.Join(deployDir, ".env"), env); err != nil {
			return nil, err
		}
	}

	// lab.env is separate so the lab container never receives the broker's
	// environment — including the paths of files it must not know exist.
	if len(p.LabEnv) > 0 {
		if err := envfile.Upsert(filepath.Join(deployDir, "lab.env"), p.LabEnv); err != nil {
			return nil, err
		}
	}

	sv := 0
	if p.Manifest.SchemaVersion != nil {
		sv = *p.Manifest.SchemaVersion
	}
	return &deployment.Entry{
		Name:          p.Manifest.Name,
		Slot:          p.Slot,
		SchemaVersion: sv,
		Files:         written,
		StackTag:      stackTag,
		InstalledAt:   time.Now().UTC(),
	}, nil
}
