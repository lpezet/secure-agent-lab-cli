package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// knownStubs are commands allowed to exist in the grammar without doing
// anything yet. It is EMPTY, and that is the point: every command `sal --help`
// lists now works.
//
// This replaced cmd/sal/testdata/script/unimplemented-commands-fail.txtar,
// which asserted that each unwritten command exited non-zero. That script did
// its job — every entry in it was deleted by the change that implemented the
// command — and once the last one went there was nothing left for it to guard.
// The rule worth keeping is the opposite one, and it is item 1 of the five
// things 1.0 means: "every command in the grammar works, or is removed from
// the grammar. A --help listing commands that exit 1 is not a 1.0."
//
// Adding a name here is therefore a deliberate step backwards from that, and
// should be argued for in the change that does it.
var knownStubs = map[string]bool{}

func TestEveryCommandInTheGrammarIsImplemented(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
		if cmd.HasSubCommands() {
			// A group's own RunE reports an unknown subcommand; the leaves are
			// what carry behaviour.
			return
		}
		if isStub(cmd) && !knownStubs[cmd.CommandPath()] {
			t.Errorf("%s is still a stub", cmd.CommandPath())
		}
	}
	walk(NewRootCmd())
}

// isStub compares function identity rather than behaviour, which is what makes
// this exact: a command wired to notImplemented is a stub no matter what its
// help text promises.
func isStub(cmd *cobra.Command) bool {
	if cmd.RunE == nil {
		return false
	}
	return reflect.ValueOf(cmd.RunE).Pointer() == reflect.ValueOf(notImplemented).Pointer()
}

// A stub that returns 0 with a friendly "coming soon" is the worst of the
// options: a script driving it appears to succeed, and in this tool "appeared
// to succeed" can mean a provider was never installed or a credential never
// stored, while the caller moves on. So notImplemented stays, and stays an
// error, for whatever is added next.
func TestAStubIsAnErrorRatherThanANoOp(t *testing.T) {
	cmd := &cobra.Command{Use: "pretend"}
	err := notImplemented(cmd, nil)
	if err == nil {
		t.Fatal("an unimplemented command must fail, not succeed quietly")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error = %q, want it to say so plainly", err)
	}
	// Naming the full path is what tells a script which invocation failed.
	if !strings.Contains(err.Error(), "pretend") {
		t.Errorf("error = %q, want the command path in it", err)
	}
}
