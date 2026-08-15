package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The rule this protects: print the URL FIRST, then attempt a browser.
//
// A launcher can hang, crash, or take the terminal with it, and it silently
// does nothing at all over SSH, in WSL and inside a dev container — which is
// this project's main path. Printing afterwards means the one useful output is
// the one most likely to be lost.
//
// Both streams share a buffer here on purpose. That is what an operator's
// terminal is, and it is the only way to observe the order: the txtar scripts
// capture stdout and stderr separately, so they cannot tell these two apart.
func TestTheURLComesBeforeAnythingThatCanFail(t *testing.T) {
	// No launcher anywhere, so the attempt reports failure — the case where
	// getting the order wrong loses the URL behind an error.
	t.Setenv("PATH", "")
	t.Setenv("BROWSER", "")

	var terminal bytes.Buffer
	printAndOpen(&terminal, &terminal, "http://127.0.0.1:49153", false)

	lines := strings.Split(strings.TrimSpace(terminal.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the URL and a note about the launcher, got:\n%s", terminal.String())
	}
	if lines[0] != "http://127.0.0.1:49153" {
		t.Errorf("first line = %q, want the URL and nothing else", lines[0])
	}
	if !strings.Contains(terminal.String(), "no browser launcher") {
		t.Errorf("a failed launch should say so:\n%s", terminal.String())
	}
}

// --no-open exists so a script can take the URL. Anything else on stdout would
// end up in whatever the script did with it.
func TestNoOpenPrintsTheURLAndNothingElse(t *testing.T) {
	var out, errOut bytes.Buffer
	printAndOpen(&out, &errOut, "http://127.0.0.1:49153", true)

	if got := out.String(); got != "http://127.0.0.1:49153\n" {
		t.Errorf("stdout = %q, want just the URL", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("--no-open should attempt nothing, but it said: %q", errOut.String())
	}
}
