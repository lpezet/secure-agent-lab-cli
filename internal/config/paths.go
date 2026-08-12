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
// Mount `secrets/`, never its parent. The parent holds a bank cache and will
// hold config besides; none of that belongs in the broker's filesystem, and a
// parent mount would put it there the moment anything new is added.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appDir is the directory name under the user's config root.
const appDir = "secure-agent-lab"

// LegacySecretsDir is where the stack currently keeps credentials. sal does not
// read or migrate it yet; it is named here so the migration has something to
// refer to rather than a string typed twice.
const LegacySecretsDir = "agent-creds"

// Dir returns the consolidated config directory, creating it 0700 if absent.
//
// 0700 on the parent as well as on secrets/ because the bank cache determines
// what gets installed into a lab. A cache another user can write to is a way to
// choose the code that ends up behind the credential boundary.
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
