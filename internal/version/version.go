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
// repo rather than by a manifest. It sat at 1.9.0 for a format reason: that is
// the release which gave bank manifests a schema_version, and below it this
// CLI cannot tell whether it understands a manifest it is reading.
//
// It sits at 1.9.2 for a different and stronger one. Every release up to and
// including 1.9.1 compares the request's host against the internal-host set
// without normalising it, so `http://BROKER:8080/<any route>` through the
// proxy reached the broker — the raw credential routes cred-gateway
// deliberately does not expose. The proxy is on both networks, so on that path
// the addon is the only control and Docker's isolation does not back it up.
// A lab pinned below this should hear about it every time sal runs.
const MinimumStack = "1.9.2"

// DefaultStack is what `sal init` pins to when nobody says otherwise.
//
// Honestly named: it is the newest release THIS BUILD knows about, not the
// newest that exists. sal does not ask the stack repo what the latest release
// is, because "newest" is a moving target and a lab's boundary should not
// depend on when it happened to be created. --stack overrides it, and the
// value is printed at init so the choice is never silent.
//
// v1.9.2 rather than v1.9.0 because of the same bypass MinimumStack names:
// pinning a NEW lab to a release with a known credential-exposure bug is the
// worst version of that default being stale.
const DefaultStack = "v1.9.2"

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
