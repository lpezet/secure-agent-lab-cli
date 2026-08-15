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
// This is a refusal, not a fallback. A pipe nobody asked for is a pipe nobody
// checked: `echo $TOKEN | sal ...` puts the credential in an argv one process
// upstream, and reading it would look like it worked. It is also how a lost
// terminal — a cron job, a CI runner, a stray `< /dev/null` — would otherwise
// stop being an error and start being an input.
//
// It is the DEFAULT that is refused, not the channel. ReadPiped below is the
// same read behind a flag, for the case where the value comes from a secret
// manager and there is no file to point at.
var ErrNotATerminal = errors.New(
	"refusing to read a credential from a pipe: stdin is not a terminal, and a piped value may be an argv one process away; " +
		"pass --from-stdin if you meant to pipe one in")

// ErrIsATerminal is returned when a piped read was asked for and stdin is a
// terminal — so nothing was piped in, and reading would block on input the
// operator has no reason to expect they owe.
var ErrIsATerminal = errors.New(
	"--from-stdin was given but stdin is a terminal, so there is nothing piped in; " +
		"pipe the value in, or drop the flag to be prompted for it")

// ReadPiped reads a credential from stdin, which the caller has explicitly
// asked for.
//
// The pair with ReadSecret is deliberate, and they live together so the rule
// is read in one place rather than re-derived: one refuses a pipe, the other
// refuses a terminal, and neither ever guesses which it has been given.
//
// max bounds the read for the same reason ResolveFile bounds a file — the
// thing on the other end of the pipe may not be a credential at all — and is
// passed in rather than imported so this package stays about terminal I/O.
func ReadPiped(max int64) ([]byte, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, ErrIsATerminal
	}

	// One byte past the limit, so exceeding it is detectable rather than
	// silently truncating a credential to exactly max bytes.
	value, err := io.ReadAll(io.LimitReader(os.Stdin, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > max {
		return nil, fmt.Errorf("refusing a credential larger than %d bytes: whatever is on stdin is unlikely to be one", max)
	}
	return value, nil
}

// FileHook is given the first line of what was typed, before any more of it is
// read, and answers whether that line named a file the operator meant to copy.
//
// It exists so that this package stays about terminal I/O while the policy —
// what counts as a path, what to ask, what to refuse — lives in
// internal/secrets, where it can be tested without a terminal.
//
// The first line is enough to decide on, in both directions: a path contains
// no newline, and a pasted PEM's first line is `-----BEGIN …`, which names no
// file. Consulting it before reading the rest is what lets a path be given at
// a multiline prompt without the blank-line ritual.
type FileHook func(firstLine []byte) (value []byte, used bool, err error)

// ReadSecret prompts and reads a credential with echo off.
//
// A multiline value — a PEM, say — is terminated by a blank line rather than
// EOF, because a terminal in no-echo mode gives no feedback that a paste
// landed and Ctrl-D behaves differently across the shells this has to work in.
//
// hook may be nil. When it is not, what was typed may name a file instead of
// being the credential, and the operator is asked which they meant — so this
// returns either what they typed or what that file holds.
//
// dest is the credential's filename, shown as "stored as <dest>". It does two
// jobs: it says which of a provider's credentials this prompt is asking for,
// when walking several, and it teaches the name that selects this one
// individually later. "stored as" rather than a bare filename because the very
// next clause offers a path as valid input, and the two would otherwise blur
// into "type this filename".
func ReadSecret(label, dest string, multiline bool, hook FileHook) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, ErrNotATerminal
	}

	source := "paste the value"
	if multiline {
		source = "paste it, then a blank line to finish"
	}
	if hook != nil {
		source += ", or the path to a file holding it"
	}
	if dest != "" {
		source = "stored as " + dest + " — " + source
	}
	fmt.Fprintf(os.Stderr, "%s\n  %s\n> ", label, source)

	if !multiline {
		value, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		value = trimEOL(value)
		if hook != nil {
			if fromFile, used, err := hook(value); err != nil || used {
				return fromFile, err
			}
		}
		return value, nil
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
		// Only the first line can be a path, and it is checked before the
		// loop asks for a second one — otherwise someone giving a path here
		// would sit at a prompt with no indication it had already been read.
		if hook != nil && len(lines) == 0 {
			fmt.Fprintln(os.Stderr)
			fromFile, used, err := hook(trimEOL(line))
			if err != nil {
				return nil, err
			}
			if used {
				return fromFile, nil
			}
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
