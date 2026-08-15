// Package config resolves the paths sal owns on the host.
//
// Everything lives under one consolidated directory. That is deliberate, and
// so is the fact that only ONE child of it is ever mounted into a container:
//
//	~/.config/secure-agent-lab/
//	  secrets/        0700 — mounted into the broker, and nothing else here is
//	  labs/<name>/    one deployment per project
//
// Nothing here is a cache. Stack content is fetched to a temporary directory
// when a command needs it and thrown away after, so everything under this
// directory is state worth keeping.
//
// Mount `secrets/`, never its parent. The parent holds every lab on this
// machine and will hold config besides; none of that belongs in the broker's
// filesystem, and a parent mount would put it there the moment anything new is
// added.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appDir is the directory name under the user's config root.
const appDir = "secure-agent-lab"

// LegacySecretsDir is where the stack currently keeps credentials.
//
// sal does not move anything out of it, and that is settled rather than
// pending: moving credentials is the worst-failing operation in this repo, and
// the population it would serve is empty. Someone with an old directory
// re-enters each credential with `sal secrets set`. This constant exists so the
// old location can be REPORTED without a string being typed twice.
const LegacySecretsDir = "agent-creds"

// Dir returns the consolidated config directory, creating it 0700 if absent.
//
// 0700 on the parent as well as on secrets/ because the labs under it hold the
// proxy addons, broker providers and gateway configs a lab runs. A deployment
// another user can write to is a way to choose the code that ends up behind the
// credential boundary.
func Dir() (string, error) {
	root, err := configRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, appDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// SecretsDir returns the directory the broker mounts. 0700, and the files
// written into it are 0600.
func SecretsDir() (string, error) {
	return subdir("secrets")
}

// LabsDir returns the root under which each project's deployment lives.
//
// Deployments live here rather than inside the project on purpose. The agent
// works in the project directory, so a deployment kept there is one the agent
// can edit — the dev-container example needs a read-only shadow mount over its
// own .devcontainer for exactly that reason. Keeping the deployment out of the
// workspace entirely is stronger than mounting it read-only: there is no mount
// to get wrong, and proxy addons, broker providers and gateway configs are not
// merely unwritable but invisible.
func LabsDir() (string, error) {
	return subdir("labs")
}

// ProvidersDir returns the root under which locally authored bank entries
// live, one directory per entry, laid out exactly like the bank's own.
//
// Out of the project for the same reason a deployment is, and it matters more
// here rather than less: an entry in this directory is code that will run
// behind the credential boundary once installed, so a scaffold inside the
// workspace would be one the agent could edit before an operator installed it.
//
// The layout is the bank's, not one of sal's own, so a provider written here
// is one that can be proposed to the bank unchanged — and so that sal reads it
// with the same code that reads the bank, rather than a second path that could
// disagree about what an entry is.
func ProvidersDir() (string, error) {
	return subdir("providers")
}

func subdir(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// configRoot honours XDG_CONFIG_HOME when it is set to an absolute path, and
// falls back to ~/.config. A relative XDG_CONFIG_HOME is ignored rather than
// resolved against the working directory: the spec says to ignore it, and
// resolving it would scatter secrets directories wherever sal happened to run.
func configRoot() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory for sal's config: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}
