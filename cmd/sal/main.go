// Command sal sets up, maintains and operates a secure-agent-lab.
//
// sal is a HOST-side tool and never ships inside the lab image. That is a
// boundary property, not an ergonomic one: `sal secrets set` and
// `sal providers add` are both boundary-widening operations, so a sal on PATH
// inside the lab would hand the agent a supported interface for widening its
// own allowlist. Do not add it to a lab image, a devcontainer feature, or a
// lab_setup fragment.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/lpezet/secure-agent-lab-cli/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// Cobra's own usage errors are already legible; anything else gets a
		// prefix so it is obvious which tool refused.
		if !errors.Is(err, errQuiet) {
			fmt.Fprintln(os.Stderr, "sal: "+err.Error())
		}
		os.Exit(1)
	}
}

var errQuiet = errors.New("")
