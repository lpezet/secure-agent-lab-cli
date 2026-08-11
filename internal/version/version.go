// Package version reports what this binary is, and what stack it expects.
package version

import (
	"runtime/debug"
	"strings"
)

// version is stamped at release time with:
//
//	-ldflags "-X github.com/lpezet/secure-agent-lab-cli/internal/version.version=v1.2.3"
//
// Left as "dev" for local builds; ReadBuildInfo fills it in for `go install`
// builds, which carry the tag in their module version.
var version = "dev"

// commit is stamped the same way; ReadBuildInfo supplies it for VCS builds.
var commit = ""

// MinimumStack is the oldest stack this build will manage without complaint.
//
// This is the WARN-level check of the three, and the only one owned by this
// repo rather than by a manifest. It sits at 1.9.0 because that is the release
// that gave bank manifests a schema_version: below it, this CLI cannot tell
// whether it understands a manifest it is reading, which is the one thing it
// must never guess at.
const MinimumStack = "1.9.0"

// CLI returns this binary's own version. It is deliberately separate from the
// stack tag: `sal --version` prints both, because "I am four versions behind"
// is ambiguous otherwise and the two-repo split would have bought nothing.
func CLI() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	if c := vcsRevision(info); c != "" {
		return "dev+" + short(c)
	}
	return version
}

// Commit returns the revision this binary was built from, or "" if unknown.
func Commit() string {
	if commit != "" {
		return short(commit)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		return short(vcsRevision(info))
	}
	return ""
}

func vcsRevision(info *debug.BuildInfo) string {
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

func short(rev string) string {
	rev = strings.TrimSpace(rev)
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
