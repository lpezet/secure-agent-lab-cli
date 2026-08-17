package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/lpezet/secure-agent-lab-cli/internal/version"

	"github.com/lpezet/secure-agent-lab-cli/internal/observer"
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
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"observerd": observerd,
			"waitfor":   waitfor,
		},
		Setup: func(e *testscript.Env) error {
			e.Setenv("FIXTURES", fixtures)

			// What `sal init` pins to when nobody says otherwise. Exported so
			// a script can assert the default without spelling the version —
			// bumping DefaultStack was otherwise a hand-edit across five
			// assertions in two files, three releases running, and every one
			// of those edits is a chance to change an assertion that meant a
			// FIXTURE's pin rather than the default.
			e.Setenv("DEFAULT_STACK", version.DefaultStack)

			// Where testscript put `sal` itself. A script that needs to
			// control PATH exactly — to prove sal copes when there is no
			// browser launcher anywhere on it, say — has to keep this entry
			// or it loses the command under test along with everything else.
			e.Setenv("SALBIN", firstPathEntry(e.Vars))

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

// observerd stands in for the observer service: it serves the audit trail as
// server-sent events at the endpoint sal reads, on a real loopback port.
//
//	observerd [-close] <file>
//
// The file holds one JSON audit event per line, and each becomes one frame.
// Without -close the connection stays open after the replay, which is what the
// real observer does — that is the shape --follow=false has to terminate
// against. With -close the stream ends, which is how a lab going away looks to
// a tail that is following.
//
// It publishes the address in FAKE_DOCKER_PORT, so the fake docker answers
// `compose port observer 9000` with it and sal reaches this server the same
// way it reaches a real one: by asking Docker which port it assigned. Nothing
// here is a seam in production code — the whole path from `docker compose
// port` through the HTTP read to the formatted line is the real one.
func observerd(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("observerd cannot be negated")
	}
	closeAfter := false
	if len(args) > 0 && args[0] == "-close" {
		closeAfter, args = true, args[1:]
	}
	if len(args) != 1 {
		ts.Fatalf("usage: observerd [-close] <events-file>")
	}
	events := ts.ReadFile(args[0])

	// Loopback only, mirroring the compose file's 127.0.0.1:: — an audit trail
	// served with no auth is only safe for not being reachable off the host,
	// and a test harness that bound wider would be practising the opposite.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ts.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != observer.EventsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, line := range strings.Split(events, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		w.(http.Flusher).Flush()
		if closeAfter {
			return
		}
		<-r.Context().Done()
	})}

	go func() { _ = srv.Serve(ln) }()
	ts.Defer(func() { _ = srv.Close() })
	ts.Setenv("FAKE_DOCKER_PORT", ln.Addr().String())
}

// waitfor blocks until a file appears.
//
//	waitfor <file>
//
// Needed because `sal observer open` deliberately does not wait for the
// launcher it starts: $BROWSER is often a browser rather than a launcher, and
// one with no window open yet runs in the foreground until it is closed, so
// waiting would hang sal for as long as the operator reads. That leaves the
// launcher's own write racing the assertion about it, and this is the test
// harness absorbing the race rather than production code changing shape to
// make a test easier.
func waitfor(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: waitfor <file>")
	}
	path := ts.MkAbs(args[0])
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			ts.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// firstPathEntry returns the head of PATH as testscript set it up, which is
// the directory holding the in-process `sal`.
func firstPathEntry(vars []string) string {
	for _, v := range vars {
		if !strings.HasPrefix(v, "PATH=") {
			continue
		}
		entries := filepath.SplitList(strings.TrimPrefix(v, "PATH="))
		if len(entries) > 0 {
			return entries[0]
		}
	}
	return ""
}
