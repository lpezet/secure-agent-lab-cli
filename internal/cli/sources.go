package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/source"
)

// newProvidersSourceCmd builds `sal providers source`.
//
// Adding a source and installing from it are two acts, and the split is the
// design rather than an extra step. Adding one says whose code may run behind
// this machine's credential boundary; installing from a source already trusted
// says nothing new. A fully-qualified repository at install time would collapse
// them, re-deciding trust every time in a long string pasted out of a README —
// and would leave nowhere to answer "which sources does this machine accept",
// which is the question worth being able to ask.
//
// Public repositories only, today. sal fetches over HTTPS the way it fetches
// the bank, so it needs no git on the machine — and a private repository needs
// a token, which is its own decision on a tool whose entire subject is not
// handling credentials carelessly. Deferred rather than guessed at.
func newProvidersSourceCmd() *cobra.Command {
	group := newGroup("source", "Places other than the bank that sal will install providers from")
	group.AddCommand(newSourceAddCmd(), newSourceListCmd(), newSourceRemoveCmd())
	return group
}

func newSourceAddCmd() *cobra.Command {
	var as, ref string
	cmd := &cobra.Command{
		Use:   "add REPO",
		Short: "Trust a repository as a place to install providers from",
		Long: "REPO is `owner/repo`, or a URL of one. The repository must mirror\n" +
			"the bank's layout — `bank/<name>/` holding provider.json and the per-service\n" +
			"directories — so that an entry written for the bank can be published unchanged\n" +
			"and read here by the same code.\n\n" +
			"This is the security decision. An entry installed from here runs behind the\n" +
			"credential boundary: its broker provider is handed your credential and decides\n" +
			"what to give the lab. sal's checks catch what is visible in the files — an\n" +
			"unexposed route whitelisted, a host declared and never matched — and cannot\n" +
			"catch what a broker provider chooses to hand back.\n\n" +
			"Public repositories only for now. Entries are fetched over HTTPS, so sal needs\n" +
			"no git on your machine; a private repository needs a token, which is a separate\n" +
			"decision not yet made.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceAdd(cmd, args[0], as, ref)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "name to install from, e.g. `--as acme` for `sal providers add slack@acme` (default: derived from the repo)")
	cmd.Flags().StringVar(&ref, "ref", "main", "tag or branch to read entries at")
	return cmd
}

func runSourceAdd(cmd *cobra.Command, repoArg, as, ref string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	owner, repo, err := source.ParseRepo(repoArg)
	if err != nil {
		return err
	}
	name := as
	if name == "" {
		name = source.DefaultName(repo)
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	reg, err := source.Load(dir)
	if err != nil {
		return err
	}

	s := source.Source{Name: name, Owner: owner, Repo: repo, Ref: ref}
	if err := reg.Add(s); err != nil {
		return err
	}

	// Read it once before recording it. A source that cannot be fetched, or
	// whose repository has no bank/ at all, is a typo far more often than it
	// is a repository that will exist later — and finding that out now costs
	// one request, while finding it out at install time costs it in the middle
	// of something else.
	entries, err := sourceEntries(cmd, s)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w.\nNothing was recorded", s, err)
	}

	if err := source.Save(dir, reg); err != nil {
		return err
	}

	fmt.Fprintf(out, "%s\n", name)
	fmt.Fprintf(out, "  repo     %s/%s\n", owner, repo)
	fmt.Fprintf(out, "  ref      %s\n", ref)
	for _, e := range entries {
		fmt.Fprintf(out, "  entry    %s@%s\n", e, name)
	}

	fmt.Fprintf(errOut, "\nAdded. `sal providers add <entry>@%s` installs from it — always qualified,\n"+
		"never by bare name, so which source an entry came from is never inferred.\n", name)
	fmt.Fprintf(errOut, "\nWhat this trusts: an entry from here runs behind your credential boundary, and\n"+
		"its broker provider decides what the lab is handed. sal checks what is visible in\n"+
		"the files and cannot check that.\n")
	return nil
}

func newSourceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Sources this machine will install providers from",
		Long: "The answer to \"whose code may run behind my credential boundary\", which is the\n" +
			"question the two-step design exists to make askable.",
		Args: cobra.NoArgs,
		RunE: runSourceList,
	}
}

func runSourceList(cmd *cobra.Command, _ []string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	reg, err := source.Load(dir)
	if err != nil {
		return err
	}
	if len(reg.Sources) == 0 {
		fmt.Fprintf(errOut, "no sources added, so providers come from the bank or from your own\n"+
			"providers directory. `sal providers source add owner/repo` adds one.\n")
		return nil
	}

	for _, s := range reg.Sources {
		fmt.Fprintf(out, "%-16s %s/%s at %s\n", s.Name, s.Owner, s.Repo, s.Ref)
	}
	fmt.Fprintf(errOut, "\n%s\n", source.Path(dir))
	return nil
}

func newSourceRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Stop trusting a source",
		Long: "Removes the source from this machine's registry. It does NOT remove anything\n" +
			"already installed from it: those files are in a deployment, recorded there, and\n" +
			"`sal providers remove NAME` in that lab is what takes them out.\n\n" +
			"Said plainly because the opposite would be a reasonable guess: untrusting a\n" +
			"source sounds like it should revoke what came from it, and a command that\n" +
			"reached into every lab on the machine to delete files is not something this\n" +
			"should do quietly.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceRemove(cmd, args[0])
		},
	}
}

func runSourceRemove(cmd *cobra.Command, name string) error {
	errOut := cmd.ErrOrStderr()

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	reg, err := source.Load(dir)
	if err != nil {
		return err
	}
	if !reg.Remove(name) {
		return fmt.Errorf("no source named %q. `sal providers source list` shows what there is", name)
	}
	if err := source.Save(dir, reg); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "removed source %s\n", name)
	fmt.Fprintf(errOut, "Anything already installed from it is untouched — `sal providers list` in a lab\n"+
		"shows what came from where, and `sal providers remove` is what takes one out.\n")
	return nil
}

// sourceEntries lists what a source offers, which doubles as the check that it
// is readable and shaped like a bank.
func sourceEntries(cmd *cobra.Command, s source.Source) ([]string, error) {
	b, tree, _, err := openSourceBank(cmd, s, "")
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	return b.List()
}

// openSourceBank reads a third-party source's bank/ at a commit.
//
// AT A COMMIT, resolved from the source's ref. The first draft of this fetched
// by ref directly and claimed that as a design choice; it is not one, because
// FetchTree takes a commit by construction — and the reasoning was wrong
// anyway. A ref is a moving pointer, so fetching by it means a branch that
// moved changes what an existing lab is compared against, which is the exact
// problem `sal` avoids everywhere else by recording the stack's commit
// alongside its tag. The resolved commit is recorded per entry at install, and
// `sal drift` passes it back here.
func openSourceBank(cmd *cobra.Command, s source.Source, commit string) (*bank.Bank, *bank.Tree, string, error) {
	opts := bankOptions(cmd)
	opts.Source = s.BankSource()

	// --stack-dir points at a stack checkout, which is not this repository.
	// Ignored here rather than reading the wrong bank.
	opts.StackDir = ""

	if commit == "" {
		var err error
		commit, err = s.BankSource().ResolveTag(cmd.Context(), s.Ref)
		if err != nil {
			return nil, nil, "", err
		}
	}

	tree, err := bank.FetchTree(cmd.Context(), commit, bank.BankSubtree, opts)
	if err != nil {
		return nil, nil, "", err
	}
	b, err := bank.OpenDir(tree.Dir)
	if err != nil {
		tree.Close()
		return nil, nil, "", err
	}
	return b, tree, commit, nil
}

// openTrustedSource resolves a source NAME through the registry.
//
// Through the registry and nowhere else: a repository named at install time
// would be a trust decision made in the middle of an install, and the refusal
// below is what keeps "which sources does this machine accept" answerable by
// `sal providers source list` rather than by reading shell history.
// commit is the one to read at, or empty to resolve the source's ref now.
// drift passes the commit recorded at install; `providers add` passes nothing,
// because choosing the version is what an install does.
func openTrustedSource(cmd *cobra.Command, name, commit string) (source.Source, *bank.Tree, *bank.Bank, string, error) {
	dir, err := config.Dir()
	if err != nil {
		return source.Source{}, nil, nil, "", err
	}
	reg, err := source.Load(dir)
	if err != nil {
		return source.Source{}, nil, nil, "", err
	}
	s, ok := reg.Find(name)
	if !ok {
		return source.Source{}, nil, nil, "", fmt.Errorf(
			"no source named %q on this machine.\n"+
				"`sal providers source add owner/repo --as %s` trusts one — adding a source is a\n"+
				"decision about whose code may run behind your credential boundary, so it is not\n"+
				"something an install does on your behalf", name, name)
	}

	b, tree, at, err := openSourceBank(cmd, s, commit)
	if err != nil {
		return source.Source{}, nil, nil, "", fmt.Errorf("cannot read %s: %w", s, err)
	}
	return s, tree, b, at, nil
}
