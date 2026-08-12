// Package prompt reads credential values from the operator.
//
// The invariant it exists to enforce: a credential value is never an argv. An
// argv is in shell history, in ps, and in any process listing the agent can
// run — so `sal secrets set` takes the name of what to set and nothing else,
// and the value arrives here.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrNotATerminal is returned when stdin is not a terminal.
//
// This is a refusal, not a fallback. Accepting a piped value would put the
// credential back in an argv one process upstream — `echo $TOKEN | sal ...` is
// the exact shape the no-argv rule exists to prevent, and it would look like it
// worked.
var ErrNotATerminal = errors.New(
	"refusing to read a credential from a pipe: stdin is not a terminal, and a piped value is an argv one process away")

// ReadSecret prompts and reads a credential with echo off.
//
// A multiline value — a PEM, say — is terminated by a blank line rather than
// EOF, because a terminal in no-echo mode gives no feedback that a paste
// landed and Ctrl-D behaves differently across the shells this has to work in.
func ReadSecret(label string, multiline bool) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, ErrNotATerminal
	}

	if multiline {
		fmt.Fprintf(os.Stderr, "%s (paste, then a blank line to finish):\n", label)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}

	if !multiline {
		value, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		return trimEOL(value), nil
	}

	var lines []string
	for {
		line, err := term.ReadPassword(fd)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return nil, err
		}
		if len(trimEOL(line)) == 0 {
			break
		}
		lines = append(lines, string(trimEOL(line)))
	}
	fmt.Fprintln(os.Stderr)
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// Line reads a non-secret value, echoed, offering a default.
//
// Unlike ReadSecret this is not refused without a terminal — the value is
// config, not a credential — but with no terminal and no default there is
// nothing to fall back to, and guessing config is how a lab ends up pointed at
// the wrong account.
func Line(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr)
		if def == "" {
			return "", fmt.Errorf("%s: no value given and no terminal to ask on", label)
		}
		return def, nil
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		if def == "" {
			return "", fmt.Errorf("%s: a value is required", label)
		}
		return def, nil
	}
	return line, nil
}

// Confirm asks a yes/no question. Unlike ReadSecret this may read a
// non-terminal stdin, because the answer is not a secret — but an absent
// terminal means no answer, so it returns the default rather than blocking.
func Confirm(question string, def bool) (bool, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return def, nil
	}
	suffix := " [y/N] "
	if def {
		suffix = " [Y/n] "
	}
	fmt.Fprint(os.Stderr, question, suffix)

	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return def, nil
}

// trimEOL drops a trailing CR that a Windows-style terminal leaves behind.
// term.ReadPassword already strips the LF.
func trimEOL(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "\r"))
}
