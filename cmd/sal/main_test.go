package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// updateScripts rewrites the txtar files from actual output. Useful when a
// deliberate change to output makes several stale at once — but read the diff
// before committing it, because it will just as happily bake in a regression.
var updateScripts = flag.Bool("update-scripts", false, "rewrite testdata scripts from actual output")

// TestMain makes `sal` available as a command inside the txtar scripts,
// running in-process so the tests stay fast and the coverage is real.
func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(m, map[string]func() int{
		"sal": run,
	}))
}

// TestScripts runs every script in testdata/script.
//
// These test the CLI as its users meet it: exit status, what lands on stdout
// versus stderr, and the file tree left behind. That is the layer where the
// grammar decisions in CLAUDE.md are actually observable — a unit test cannot
// tell you that `sal observer disable` is not a command.
func TestScripts(t *testing.T) {
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}

	testscript.Run(t, testscript.Params{
		Dir:           filepath.Join("testdata", "script"),
		UpdateScripts: *updateScripts,
		Setup: func(e *testscript.Env) error {
			e.Setenv("FIXTURES", fixtures)

			// Point HOME at the script's scratch directory.
			//
			// Not hygiene — a safety property. The consolidated secrets
			// directory lives under $HOME, and a test run that reached the
			// operator's real one could overwrite a live credential. A test
			// suite for this tool in particular has no business being able to
			// do that.
			e.Setenv("HOME", e.WorkDir)
			e.Setenv("XDG_CONFIG_HOME", filepath.Join(e.WorkDir, ".config"))
			return nil
		},
	})
}
