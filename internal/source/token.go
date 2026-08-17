package source

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// EnvVars are the environment variables checked for a token, in order.
//
// Named here and interpolated everywhere else — messages, help text and tests
// all read these rather than spelling them out. Partly so there is one place
// to change, and partly because internal/invariants forbids a bank entry name
// as a string literal, the bank has an entry called `github`, and GITHUB_TOKEN
// contains it. The variable names the hosting service's convention and has
// nothing to do with that provider; exempting two short constants is honest,
// while exempting every sentence that mentions one would go quiet the first
// time somebody reworded a message.
var EnvVars = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// Token finds the credential the operator already has for reading their own
// repositories, so a private source works without sal storing anything.
//
// This does NOT contradict `sal secrets set` refusing a value in argv. That
// rule is about what the credential DOES: a provider credential is stored on
// disk, mounted into the broker and injected into the agent's traffic — it is
// the thing the boundary exists to protect. This one authenticates one HTTPS
// GET made by the operator's own tool, on the operator's own machine, with the
// operator's own authority. It is never written down, never mounted, and never
// reaches the lab. `git clone` would use exactly the same credential.
//
// Order is explicit before ambient:
//
//  1. GITHUB_TOKEN, then GH_TOKEN — what CI sets, and what somebody who wants
//     a specific identity used will set deliberately.
//  2. `gh auth token` — what a developer machine usually has, held in a
//     keychain rather than an environment variable.
//
// That order matters when both exist. An environment variable is the one the
// operator typed for this run, and the keychain is whatever they happened to
// log in as months ago; silently preferring the second would make the first
// look ignored.
func Token(ctx context.Context) string {
	for _, name := range EnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ghToken(ctx)
}

// ghToken asks the GitHub CLI, if it is installed.
//
// Best-effort, and silent when it fails: not having `gh` is the ordinary case,
// and a public source needs no token at all. The failure that matters — a
// private repository with no credential anywhere — surfaces as a 404 from the
// fetch, which is what GitHub returns for a repository you cannot see, and
// that is where the message about it belongs.
func ghToken(ctx context.Context) string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, path, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TokenSource names where a token came from, for reporting. Never the token.
func TokenSource(ctx context.Context) string {
	for _, name := range EnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return "$" + name
		}
	}
	if ghToken(ctx) != "" {
		return "gh auth token"
	}
	return ""
}
