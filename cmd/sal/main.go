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
	"fmt"
	"os"

	"github.com/lpezet/secure-agent-lab-cli/internal/cli"
)

func main() {
	os.Exit(run())
}

// run is main's body, split out so testscript can call it in-process. Keeping
// the exit code as a return value rather than an os.Exit is what makes the
// CLI's observable behaviour — stdout, stderr, status — testable at all.
func run() int {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sal: "+err.Error())
		return 1
	}
	return 0
}
