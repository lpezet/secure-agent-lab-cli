package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
)

// stackContext separates two versions that are easy to conflate and are not
// the same question.
//
// BankTag is which release to READ the bank at. LabTag is what the deployment
// actually RUNS. They are equal by default, and differ the moment someone
// passes --stack to look at another release — at which point "can I install
// this?" is answered by the lab's version, not by the bank's. Comparing an
// entry's min_stack against the release it was published in is self-referential
// and would report every entry as installable.
type stackContext struct {
	BankTag string
	LabTag  string // "" when not run inside a lab
	LabRoot string
}

// resolveStack decides which release to read the bank at.
//
// The default is the tag the lab in this directory is pinned to, never "the
// newest available". A command that silently read a newer bank than the lab
// runs would offer entries whose min_stack the deployment does not satisfy,
// and that failure lands at runtime inside a container rather than here.
func resolveStack(override string) (stackContext, error) {
	var sc stackContext

	root, rec, err := deployment.Find(cwd())
	if err == nil {
		sc.LabRoot = root
		sc.LabTag = rec.StackTag
	}

	switch {
	case override != "":
		sc.BankTag = override
	case sc.LabTag != "":
		sc.BankTag = sc.LabTag
	case err != nil:
		return sc, fmt.Errorf("%w; pass --stack to name a release explicitly", err)
	default:
		return sc, fmt.Errorf("%s records no stack tag; pass --stack to name one", root)
	}
	return sc, nil
}

// openBank fetches the bank at a tag, honouring the global --offline and
// --refresh flags.
func openBank(cmd *cobra.Command, tag string) (*bank.Bank, error) {
	offline, _ := cmd.Flags().GetBool("offline")
	refresh, _ := cmd.Flags().GetBool("refresh")

	return bank.Open(cmd.Context(), tag, bank.Options{
		Offline: offline,
		Refresh: refresh,
	})
}
