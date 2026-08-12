package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/prompt"
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

	// BankCommit is what BankTag resolves to. Fetches use this, never the tag:
	// a tag is a mutable pointer, and for an existing lab the commit is
	// already in the install record, so nothing needs to be asked for.
	BankCommit string

	LabTag  string // "" when not run inside a lab
	LabRoot string
}

// resolveStack decides which release to read the bank at.
//
// The default is the release the lab in this directory was built from, never
// "the newest available". A command that silently read a newer bank than the
// lab runs would offer entries whose min_stack the deployment does not
// satisfy, and that failure lands at runtime inside a container rather than
// here.
func resolveStack(cmd *cobra.Command, override string) (stackContext, error) {
	var sc stackContext

	l, ptr, findErr := lab.Find(cwd())
	var rec *deployment.Record
	if findErr == nil {
		sc.LabRoot = l.Dir
		sc.LabTag = ptr.StackTag
		// The deployment's own record beats the project's pointer: it says
		// what was built rather than what was asked for.
		if r, err := deployment.Load(l.Dir); err == nil {
			rec = r
			if r.StackTag != "" {
				sc.LabTag = r.StackTag
			}
		}
	}

	switch {
	case override != "":
		sc.BankTag = override
	case sc.LabTag != "":
		sc.BankTag = sc.LabTag
		if rec != nil {
			sc.BankCommit = rec.StackCommit
		}
	case findErr != nil:
		return sc, fmt.Errorf("%w; pass --stack to name a release explicitly", findErr)
	default:
		return sc, fmt.Errorf("%s records no stack tag; pass --stack to name one", sc.LabRoot)
	}

	// Only resolve when the answer is not already known. An existing lab
	// records its commit, and a local checkout has no tag to resolve.
	if sc.BankCommit == "" && stackDir(cmd) == "" {
		commit, err := bank.DefaultSource().ResolveTag(cmd.Context(), sc.BankTag)
		if err != nil {
			return sc, err
		}
		sc.BankCommit = commit
	}
	return sc, nil
}

// stackDir is a local checkout of the stack repo to read instead of
// downloading.
//
// This replaced an --offline flag, and is better than it in every direction:
// it names where the content came from instead of depending on whatever a
// hidden cache happened to hold, it works on a machine that has never had
// network access, and it lets someone test an unreleased branch.
func stackDir(cmd *cobra.Command) string {
	dir, _ := cmd.Flags().GetString("stack-dir")
	return dir
}

func bankOptions(cmd *cobra.Command) bank.Options {
	return bank.Options{StackDir: stackDir(cmd)}
}

// openBank fetches the bank subtree. The caller closes the returned tree.
func openBank(cmd *cobra.Command, commit string) (*bank.Bank, *bank.Tree, error) {
	return bank.Open(cmd.Context(), commit, bankOptions(cmd))
}

// confirm asks a yes/no question through internal/prompt, which returns the
// default rather than blocking when there is no terminal — so a scripted run
// declines a destructive action instead of hanging on a prompt nobody sees.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	return prompt.Confirm(question, false)
}

// commitFor returns the commit a deployment was built from.
//
// Falls back to resolving the tag for a record that predates commit recording,
// or one created against a local checkout where there was no commit to record.
func commitFor(cmd *cobra.Command, rec *deployment.Record) (string, error) {
	if rec.StackCommit != "" || stackDir(cmd) != "" {
		return rec.StackCommit, nil
	}
	if rec.StackTag == "" {
		return "", fmt.Errorf("this deployment records neither a stack commit nor a tag")
	}
	return bank.DefaultSource().ResolveTag(cmd.Context(), rec.StackTag)
}
