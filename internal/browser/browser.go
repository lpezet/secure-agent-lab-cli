// Package browser hands a URL to whatever on this machine can display one.
//
// It is best-effort by nature, and the caller must treat it that way: a
// launcher can exit 0 having done nothing at all. That is the ordinary case
// over SSH, in WSL and inside a dev container, which is why every caller in
// this repo prints the URL before calling this, never after.
package browser

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrNoLauncher means nothing on PATH could be asked to open a URL.
var ErrNoLauncher = errors.New("no browser launcher on PATH")

// Open starts a launcher for url and reports the command it used.
//
// It deliberately does NOT wait: $BROWSER is often a browser rather than a
// launcher, and one with no window open yet runs in the foreground until it
// is closed. Waiting would hang sal for as long as the operator reads. The
// exit status would not be worth much either — the failure this cannot see is
// a launcher that succeeds and displays nothing.
//
// It also deliberately takes no context. The browser is the operator's, not
// sal's: tying it to sal's lifetime would mean a window that closes when the
// command that opened it returns.
func Open(url string) (string, error) {
	for _, argv := range candidates(url) {
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, argv[1:]...)
		// Discarded, not inherited. A launcher that chattered on stdout would
		// land in the middle of output a caller may be piping somewhere.
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			continue
		}
		// Reaped in the background rather than left as a zombie, for the
		// caller that keeps running after opening something.
		go func() { _ = cmd.Wait() }()
		return strings.Join(argv, " "), nil
	}
	return "", ErrNoLauncher
}

// candidates lists what to try, in order.
//
// $BROWSER comes first everywhere, because it is the operator saying what they
// want and it is the only entry that works on a machine where the usual
// launcher is missing or useless. The convention it follows is the de facto
// one: a colon-separated list of commands, each optionally containing %s where
// the URL goes.
func candidates(url string) [][]string {
	var out [][]string

	for _, entry := range strings.Split(os.Getenv("BROWSER"), ":") {
		fields := strings.Fields(entry)
		if len(fields) == 0 {
			continue
		}
		if substituted := substitute(fields, url); substituted != nil {
			out = append(out, substituted)
			continue
		}
		out = append(out, append(fields, url))
	}

	switch runtime.GOOS {
	case "darwin":
		out = append(out, []string{"open", url})
	case "windows":
		out = append(out, []string{"rundll32", "url.dll,FileProtocolHandler", url})
	default:
		// wslview first: on WSL xdg-open frequently exists and opens nothing,
		// which is the silent failure this whole design is shaped around.
		out = append(out, []string{"wslview", url}, []string{"xdg-open", url})
	}
	return out
}

// substitute replaces %s with the URL, returning nil if there was none.
func substitute(fields []string, url string) []string {
	found := false
	replaced := make([]string, len(fields))
	for i, f := range fields {
		if strings.Contains(f, "%s") {
			found = true
			f = strings.ReplaceAll(f, "%s", url)
		}
		replaced[i] = f
	}
	if !found {
		return nil
	}
	return replaced
}
