package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/selfupdate"
	"github.com/lpezet/secure-agent-lab-cli/internal/version"
)

// newUpdateCmd is `sal update` — the BINARY, never the lab.
//
// The two words are close enough to be confused, and confusing them is
// expensive in one direction: `sal upgrade` moves a lab's security boundary.
// So each command's help names the other, and neither goes near the other's
// job. They are on separate lines on purpose — if updating the CLI also moved
// the stack, upgrading your tool would silently move everyone's boundary, and
// pinning your boundary would mean running a stale CLI forever.
func newUpdateCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the sal binary itself",
		Long: "Replaces this sal with the newest published release, verifying it exactly\n" +
			"the way the install script does: the checksum always, and the signature over\n" +
			"it when cosign is on PATH. A missing checksum, a checksums file with no line\n" +
			"for this build, or a signature that does not verify are refusals.\n\n" +
			"This updates the BINARY. `sal upgrade` is the one that moves a lab to a newer\n" +
			"stack release — they are versioned separately, so that updating your tool\n" +
			"cannot move your security boundary.\n\n" +
			"There is deliberately no version argument. The install script already takes\n" +
			"one, and a second way to pin the binary is a second thing to get wrong:\n" +
			"  curl -fsSL " + installScriptURL + " | bash -s v0.2.0",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false,
		"report whether a newer release exists and change nothing")
	return cmd
}

// installScriptURL is where the other install path lives. Named once so the
// help text and any message can interpolate it.
const installScriptURL = "https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh"

func runUpdate(cmd *cobra.Command, checkOnly bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	cfg := &selfupdate.Config{Cosign: selfupdate.FindCosign()}

	latest, err := cfg.Resolve(cmd.Context(), "latest")
	if err != nil {
		return err
	}

	current := version.CLI()
	if sameRelease(current, latest) {
		// Not a failure, and not silent either: "nothing to do" is the answer
		// somebody ran this to get.
		fmt.Fprintf(out, "sal %s is the newest release\n", tagged(current))
		return nil
	}

	// A locally built sal is not a release and has no business being replaced
	// by one without a word — someone who ran `make build` is usually testing a
	// change, and silently overwriting it would throw that away.
	if isDevBuild(current) {
		fmt.Fprintf(errOut, "note: this is a development build (%s), not a release\n", current)
	}

	// Printed before anything is downloaded, so --check and a real update agree
	// about what they are talking about.
	fmt.Fprintf(out, "%s is available (this is %s)\n", latest, tagged(current))
	if checkOnly {
		fmt.Fprintf(errOut, "run `sal update` to install it\n")
		return nil
	}

	// Resolved BEFORE the download, so a sal that cannot work out where it
	// lives fails having changed nothing.
	self, err := selfupdate.Self()
	if err != nil {
		return fmt.Errorf("cannot work out which binary is running, so there is nothing safe to replace: %w", err)
	}

	bin, signed, err := cfg.Fetch(cmd.Context(), latest)
	if err != nil {
		return err
	}
	if signed {
		fmt.Fprintf(errOut, "signature verified\n")
	} else {
		// The same words install.sh uses, because it is the same state and
		// somebody comparing the two should not have to wonder.
		fmt.Fprintf(errOut, "cosign not found — checksum verified, signature NOT checked.\n")
		fmt.Fprintf(errOut, "  to check it: %s/releases/tag/%s\n", releasesURL, latest)
	}

	if err := selfupdate.Replace(self, bin); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "installed %s at %s\n", latest, self)
	return nil
}

// releasesURL is this repo's release page, for the message that tells someone
// how to check a signature this run could not.
const releasesURL = "https://github.com/lpezet/secure-agent-lab-cli"

// sameRelease compares this binary's stamped version with a release tag.
//
// They are NOT spelled the same, and that is the whole reason this exists:
// GoReleaser stamps {{ .Version }}, which has no leading v, while a release tag
// has one. Comparing them raw reports an up-to-date sal as behind and then
// replaces it with a byte-identical copy of itself, every single run — a bug
// that costs a download and looks like the command not working.
func sameRelease(current, tag string) bool {
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(tag, "v")
}

// tagged spells a version the way a tag does, so one line cannot say 0.2.1 and
// the next v0.2.1 about the same thing.
func tagged(v string) string {
	if isDevBuild(v) {
		return v
	}
	return "v" + strings.TrimPrefix(v, "v")
}

// isDevBuild reports a binary that did not come from a release. version.CLI()
// answers "dev", or "dev+<commit>" when it could read the build info.
func isDevBuild(v string) bool {
	return v == "dev" || strings.HasPrefix(v, "dev+")
}
