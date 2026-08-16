// Package version reports what this binary is, and what stack it expects.
package version

import (
	"runtime/debug"
	"strings"

	"github.com/lpezet/secure-agent-lab-cli/internal/stackver"
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
// It moved to v1.9.2 for the bypass MinimumStack names — pinning a NEW lab to
// a release with a known credential-exposure bug is the worst version of a
// stale default — then to v1.10.1 once sal could create a deployment shaped
// the way that release expects (see AddonsBakedFrom), and to v1.12.0 with
// TemplateFrom, which is also the oldest release a new lab can be created at.
const DefaultStack = "v1.12.0"

// AddonsBakedFrom is the first stack release whose proxy IMAGE carries the
// base addons — 000_policy.py and 001_allowlist.py — at
// /opt/agent-proxy/addons/, loaded ahead of the /addons mount a deployment
// provides.
//
// Below it, a deployment that does not vendor them has no internal-host block
// at all: the proxy sits on both networks, so with no policy addon loaded it
// forwards to the broker and the cred-gateway whitelist can be walked around
// entirely. That is the hole the stack closed by baking them in.
//
// At or above it, vendoring is the opposite of a control: the image's copy
// wins, and the deployment's is skipped with a warning naming the file.
const AddonsBakedFrom = "1.10.0"

// TemplateFrom is the oldest release whose deployment template sal can use.
//
// A floor rather than a warning, and it is not about age. The template arrived
// at 1.10.0 as `template/`, moved to `template/deployment/` at 1.11.0, and
// named five specific bank entries in the broker's `environment:` block until
// 1.12.0 — where `environment:` wins over `env_file:`, so those values would
// have overridden what a manifest declares about its own credential's path.
//
// sal uses the file VERBATIM, which is what removed the last copy of this
// stack's wiring from this repo. That is only correct from the release where
// the file stopped saying anything about specific providers, so creating a lab
// below it is refused rather than half-supported.
const TemplateFrom = "1.12.0"

// StackHasUsableTemplate reports whether a release ships a deployment template
// sal can use as it stands. An unparseable tag answers false: a lab that
// cannot be created is a clear failure, where one created from a template that
// names providers it does not have is a quiet one.
func StackHasUsableTemplate(tag string) bool {
	have, err := stackver.Parse(tag)
	if err != nil {
		return false
	}
	from, err := stackver.Parse(TemplateFrom)
	if err != nil {
		return false
	}
	return !have.Less(from)
}

// StackBakesAddons reports whether a release carries the base proxy addons in
// its image, so a deployment must not vendor them.
//
// A tag this cannot parse answers false, which vendors them. That is the
// direction that fails closed — a vendored copy at a release that bakes them
// is dead weight, while a missing one below that release is a missing control
// — and it is the same choice scripts/check-drift.sh makes for a pin that is
// not a release tag.
func StackBakesAddons(tag string) bool {
	have, err := stackver.Parse(tag)
	if err != nil {
		return false
	}
	from, err := stackver.Parse(AddonsBakedFrom)
	if err != nil {
		return false
	}
	return !have.Less(from)
}

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
