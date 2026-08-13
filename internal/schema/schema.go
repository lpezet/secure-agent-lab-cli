// Package schema states the compatibility rule for the files sal writes.
//
// Two of them outlive the build that wrote them, and both are contracts:
//
//   - <lab>/.sal/installed.json is read by scripts/check-drift.sh in the STACK
//     repo, by someone who may never have installed sal. Cross-repo.
//   - <project>/.sal/lab.json is committed into a user's git history and read
//     by whatever sal they have next month. Cross-version.
//
// The rule is the stack's own, adopted for the same reasons it was adopted
// there for bank manifests in v1.9.0: support a fixed set of generations and
// REFUSE anything above it. A file from the future may carry a field this
// build does not know to honour, and acting on the part it understands while
// ignoring the rest is how a control gets silently dropped.
//
// An integer, not a semver. This is a compatibility generation, and a minor
// number invites "close enough" — which is the judgement call the field exists
// to remove.
package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// Current is what this build writes.
const Current = 1

// Supported is every generation this build can read.
var Supported = []int{1}

// Check validates a generation read from a file.
//
// An absent version is refused rather than assumed to be generation 1. sal has
// written both formats since before they carried the field, and treating
// absence as "probably the oldest" would make that guess permanent — every
// future reader carrying the same branch forever. Refusing keeps the rule one
// sentence long.
func Check(what string, version int, path string) error {
	if version == 0 {
		return fmt.Errorf(
			"%s at %s declares no schema_version, so it predates this build's format and cannot be read safely",
			what, path)
	}
	for _, ok := range Supported {
		if version == ok {
			return nil
		}
	}
	return fmt.Errorf(
		"%s at %s is generation %d, which this build does not understand (it supports %s); "+
			"it may carry something this sal would silently ignore, so it is refused rather than read — upgrade sal",
		what, path, version, join(Supported))
}

func join(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}
