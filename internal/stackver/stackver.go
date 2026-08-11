// Package stackver compares stack versions.
//
// It exists because the same version is spelled two ways across the contract:
// a manifest's min_stack is bare ("1.7.0", per its ^[0-9]+\.[0-9]+\.[0-9]+$
// pattern) while the thing it is compared against is a git tag ("v1.9.0").
// Normalizing in one tested place is cheaper than being bitten once by a
// comparison that silently read "v1" as unparseable and passed.
package stackver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a three-part stack version. The stack tags only ever X.Y.Z, so
// there is deliberately no pre-release or build-metadata handling here: a tag
// that carries one is something this code should refuse rather than guess at.
type Version struct {
	Major, Minor, Patch int
}

// Parse accepts either spelling — "1.9.0" or "v1.9.0" — and rejects anything
// else.
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "v")

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("not an X.Y.Z version: %q", s)
	}

	var v Version
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if parts[i] == "" {
			return Version{}, fmt.Errorf("not an X.Y.Z version: %q", s)
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("not an X.Y.Z version: %q", s)
		}
		*dst = n
	}
	return v, nil
}

// Compare returns -1 if v sorts before o, 0 if equal, +1 if after.
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{
		{v.Major, o.Major},
		{v.Minor, o.Minor},
		{v.Patch, o.Patch},
	} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

// String renders the bare spelling, without the tag's leading "v".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Tag renders the git-tag spelling.
func (v Version) Tag() string { return "v" + v.String() }
