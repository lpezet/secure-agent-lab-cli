package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
	"github.com/lpezet/secure-agent-lab-cli/internal/prompt"
	"github.com/lpezet/secure-agent-lab-cli/internal/secrets"
)

// newSecretsCmd builds the `sal secrets` group.
//
// Secrets live at ~/.config/secure-agent-lab/secrets/, 0700 on the directory
// and 0600 on the files. The broker mounts THAT directory and never its
// parent: the parent also holds the labs themselves, and none of that belongs
// anywhere near the broker.
//
// Nothing in this file knows what credentials any particular provider has. It
// reads the bank entry's manifest and walks whatever `secrets` declares — which
// is why `sal secrets set anthropic` can ask for an OAuth token and an API key,
// in that precedence, without this CLI having heard of either. The prompts are
// the bank author's words, and the bank author is the only person who knows
// the provider's rule.
func newSecretsCmd() *cobra.Command {
	group := newGroup("secrets", "Store credentials for this machine's labs")
	group.AddCommand(newSecretsSetCmd(), newSecretsListCmd())
	return group
}

func newSecretsSetCmd() *cobra.Command {
	var (
		fromFile, stack string
		fromStdin       bool
	)
	cmd := &cobra.Command{
		Use:   "set PROVIDER [FILE]",
		Short: "Store a credential, read from the terminal with echo off",
		Long: "Walks the credentials the provider's manifest declares, prompting with the\n" +
			"bank author's own words. With no FILE it asks for every one of them; with a\n" +
			"FILE it sets that one.\n\n" +
			"A credential is named by its FILE — the name `sal secrets list` prints and\n" +
			"the name on disk — never by the environment variable the manifest pairs it\n" +
			"with. That variable is what the BROKER reads, and its value is a path inside\n" +
			"the container, so it names neither the credential nor anywhere you can put\n" +
			"one. Naming one gets you told which file it means.\n\n" +
			"The value is read from the terminal with echo off. There is no flag to pass the\n" +
			"value, and there will not be one: an argv is in shell history, in ps, and in\n" +
			"any process listing the agent can run.\n\n" +
			"--from-file and --from-stdin are not exceptions to that. Three exposures, not\n" +
			"one:\n" +
			"  a PATH in an argv reveals nothing,\n" +
			"  a VALUE in an argv is the whole problem,\n" +
			"  and stdin is not an argv at all.\n" +
			"Typing a path at the prompt works too, and is confirmed before anything is\n" +
			"copied.\n\n" +
			"A pipe with NEITHER flag is still refused. --from-stdin is for a value with no\n" +
			"file to point at — `op read ... | sal secrets set ... --from-stdin` — and being\n" +
			"explicit is what keeps a lost terminal, in cron or CI, an error rather than an\n" +
			"input source.\n\n" +
			"Credentials are shared by every lab on this machine, so overwriting one\n" +
			"rotates it for all of them. That is stated and confirmed, never assumed.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) > 1 {
				selector = args[1]
			}
			return runSecretsSet(cmd, args[0], selector, fromFile, stack, fromStdin)
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read the credential from a file instead of prompting")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "read the credential from stdin, for a secret manager with no file to point at")
	cmd.Flags().StringVar(&stack, "stack", "", "read the manifest at this release instead of the one this lab is pinned to")
	return cmd
}

func newSecretsListCmd() *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "list [PROVIDER]",
		Short: "List stored credentials by name, never by value",
		Long: "Reports which credentials are set, which are missing, and which files in the\n" +
			"secrets directory no installed provider claims.\n\n" +
			"That last one is the reason this is a control rather than a listing: an\n" +
			"unclaimed credential is a live secret mounted into every broker on this\n" +
			"machine that nothing references.\n\n" +
			"It never prints a value, a length, or a fingerprint.",
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runSecretsList(cmd, name, stack)
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "read manifests at this release instead of the one this lab is pinned to")
	return cmd
}

func runSecretsSet(cmd *cobra.Command, provider, selector, fromFile, stack string, fromStdin bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	// Naming two sources is a mistake, not a precedence question. Picking one
	// would mean the operator's other source was silently ignored — and which
	// bytes ended up in the credential file is the thing this command cannot
	// afford to leave to a rule nobody read.
	if fromFile != "" && fromStdin {
		return errors.New("--from-file and --from-stdin name two sources for one credential; pass one of them")
	}

	sc, err := resolveStack(cmd, stack)
	if err != nil {
		return err
	}
	b, tree, err := openBank(cmd, sc.BankCommit)
	if err != nil {
		return err
	}
	defer tree.Close()

	m, err := b.Manifest(provider)
	if err != nil {
		return err
	}
	// Same order as every other command that reads a manifest, and for the
	// same reason: a generation this build does not understand may declare a
	// control it would silently skip.
	if err := m.CheckSchemaVersion(); err != nil {
		return err
	}
	if len(m.Secrets) == 0 {
		return fmt.Errorf("%s declares no credentials", m.Name)
	}

	chosen, err := selectSecrets(m, selector)
	if err != nil {
		return err
	}
	// A non-interactive source sets exactly one credential. Without this the
	// value would be written into every credential the provider declares —
	// one source, several destinations, and no prompt in between to notice.
	if source := nonInteractiveSource(fromFile, fromStdin); source != "" && len(chosen) != 1 {
		return fmt.Errorf("%s sets one credential, but %s declares %d; name which one:\n%s",
			source, m.Name, len(chosen), secretMenu(m))
	}

	dir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	store := secrets.Store{Dir: dir}

	users, err := labsUsing(provider)
	if err != nil {
		return err
	}

	for _, s := range chosen {
		value, err := readSecretValue(cmd, s, fromFile, fromStdin)
		if err != nil {
			return err
		}
		if len(value) == 0 {
			if !s.Optional {
				return fmt.Errorf("%s is required", s.File)
			}
			fmt.Fprintf(errOut, "left %s unset (optional)\n", s.File)
			continue
		}

		// The rotation warning. Credentials are machine-wide, so this is not a
		// question about this project — every lab that installed the provider
		// reads this same file, and they all move together.
		if st := store.Stat(s.File); st.Set {
			fmt.Fprintf(errOut, "%s is already set (modified %s)%s\n",
				s.File, st.Modified.Format("2006-01-02 15:04"), sharedBy(users))
			ok, err := prompt.Confirm("overwrite it?", false)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(errOut, "left %s alone\n", s.File)
				continue
			}
		}

		if err := store.Write(s.File, secrets.Normalize(value, s.Multiline)); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s (%04o)\n", filepath.Join(dir, s.File), secrets.Perm)
	}
	return nil
}

// nonInteractiveSource names the flag in play, or "" when the operator is
// being prompted. Both flags carry the same rule, so both have to answer to
// the same check rather than each remembering it.
func nonInteractiveSource(fromFile string, fromStdin bool) string {
	switch {
	case fromFile != "":
		return "--from-file"
	case fromStdin:
		return "--from-stdin"
	}
	return ""
}

// readSecretValue gets one credential's bytes, from a flag or the terminal.
func readSecretValue(cmd *cobra.Command, s manifest.Secret, fromFile string, fromStdin bool) ([]byte, error) {
	errOut := cmd.ErrOrStderr()

	// Stdin is not an argv, which is the whole distinction: a secret manager
	// writes to a pipe and never to a command line. What this does NOT relax is
	// the default — a pipe with no flag is still refused in ReadSecret, so a
	// lost terminal stays an error instead of quietly becoming an input.
	if fromStdin {
		return prompt.ReadPiped(secrets.MaxFileSize)
	}

	if fromFile != "" {
		ref := secrets.ResolveFile([]byte(fromFile))
		if ref == nil {
			return nil, fmt.Errorf("--from-file %s: no such file", fromFile)
		}
		if !ref.Readable() {
			return nil, fmt.Errorf("--from-file %s", ref.Describe())
		}
		noteLooseSource(errOut, ref)
		return ref.Read()
	}

	return prompt.ReadSecret(s.Prompt, s.File, s.Multiline, fileHook(cmd, s.File))
}

// fileHook implements the "did you mean this file?" question.
//
// It is what makes typing a path at the prompt work, and it exists mostly for
// the direction that currently fails silently: someone pasting
// ~/Downloads/key.pem into a prompt that asked for a key. Without the check,
// sal writes the path as though it were the credential and reports success,
// and the failure surfaces much later as a 401 with nothing attached to it.
//
// Defaulting to yes is deliberate. A pasted path is an ordinary slip; a
// genuine credential that also names an existing file on this machine is not a
// case worth designing for.
func fileHook(cmd *cobra.Command, dest string) prompt.FileHook {
	errOut := cmd.ErrOrStderr()
	return func(firstLine []byte) ([]byte, bool, error) {
		ref := secrets.ResolveFile(firstLine)
		if ref == nil {
			return nil, false, nil
		}

		// The line below is the only echo this prompt gives: the operator
		// typed blind, and this is where they find out what landed. Printing a
		// resolved PATH is safe — it only ever appears because the input named
		// a file that exists.
		fmt.Fprintf(errOut, "that resolves to a file: %s\n", ref.Describe())
		if !ref.Readable() {
			// Not silently treated as the value: a permissions typo must not
			// become a credential file containing a path.
			return nil, false, fmt.Errorf("cannot copy %s", ref.Path)
		}

		ok, err := prompt.Confirm(fmt.Sprintf("copy its contents into %s?", dest), true)
		if err != nil || !ok {
			return nil, false, err
		}
		noteLooseSource(errOut, ref)
		body, err := ref.Read()
		return body, true, err
	}
}

// noteLooseSource says so when a credential is copied out of a file other
// users can read. Not a refusal — the operator's own key is theirs to keep how
// they like — but sal writes 0600 and the source keeps whatever it had, so
// staying quiet would imply a tightening that did not happen.
func noteLooseSource(w io.Writer, ref *secrets.FileRef) {
	if ref.LooseSource() {
		fmt.Fprintf(w, "note: %s is readable by other users on this machine, and stays that way\n", ref.Path)
	}
}

// selectSecrets picks which of a manifest's credentials to prompt for.
//
// No selector means all of them, which is the hand-held path: `sal secrets set
// anthropic` asks for the OAuth token and then the API key, in the manifest's
// order, with the manifest's wording.
//
// A selector is a credential's FILE, spelled exactly. Not its env var, and not
// any part of either — see refuseByEnv for why the env name is refused with an
// explanation rather than simply not matching.
//
// Exact-only is the point, not a limitation. Validate rejects a manifest whose
// secrets share a `file`, so an exact match on a unique field is unambiguous
// BY CONSTRUCTION and there is no tie to break. An earlier version matched
// substrings and refused ambiguity, which worked but put the riskiest decision
// in this file behind a heuristic: resolving a tie wrong writes a token into
// the file the broker reads as an API key, silently at install and visibly
// only when a request is rejected. Now there is nothing to resolve.
func selectSecrets(m *manifest.Manifest, selector string) ([]manifest.Secret, error) {
	if selector == "" {
		return m.Secrets, nil
	}

	for _, s := range m.Secrets {
		// EqualFold rather than ==, and safe because Validate compares files
		// case-insensitively too, so two that differ only in case cannot both
		// be declared.
		if strings.EqualFold(s.File, selector) {
			return []manifest.Secret{s}, nil
		}
	}
	return nil, refuseByEnv(m, selector)
}

// refuseByEnv builds the refusal for a selector that named no credential.
//
// The env name gets its own message. Someone who read a deployment's .env and
// reasoned backwards will type ANTHROPIC_AUTH_TOKEN_PATH, and "no credential
// matching …" would tell them the credential does not exist — worse than the
// confusion that made this the file's job in the first place. Four things wear
// that one name: the env var, its value (a path INSIDE the container), the
// host file, and the credential itself. Only the last is what they have.
func refuseByEnv(m *manifest.Manifest, selector string) error {
	for _, s := range m.Secrets {
		if strings.EqualFold(s.Env, selector) {
			return fmt.Errorf(
				"%s is the environment variable the broker reads, and its value is a path inside the container.\n"+
					"You are setting the credential itself — name it by its file:\n  %s — %s",
				s.Env, s.File, s.Prompt)
		}
	}
	return fmt.Errorf("%s declares no credential named %q; name one of:\n%s", m.Name, selector, secretMenu(m))
}

// secretMenu lists the credentials by the name used to select them, paired
// with the manifest's own wording — the same two columns the interactive
// prompt shows, so what is read is what is typed.
func secretMenu(m *manifest.Manifest) string {
	var b strings.Builder
	for _, s := range m.Secrets {
		fmt.Fprintf(&b, "  %s — %s\n", s.File, s.Prompt)
	}
	return strings.TrimRight(b.String(), "\n")
}

func runSecretsList(cmd *cobra.Command, provider, stack string) error {
	out := cmd.OutOrStdout()

	dir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	store := secrets.Store{Dir: dir}
	fmt.Fprintf(out, "%s\n\n", dir)

	sc, err := resolveStack(cmd, stack)
	if err != nil {
		return err
	}
	b, tree, err := openBank(cmd, sc.BankCommit)
	if err != nil {
		return err
	}
	defer tree.Close()

	names := []string{provider}
	if provider == "" {
		// Everything this machine's labs have installed, rather than the whole
		// bank: the question is "what do my labs need", not "what exists".
		if names, err = installedEverywhere(); err != nil {
			return err
		}
	}

	claimed := map[string]bool{}
	for _, name := range names {
		m, err := b.Manifest(name)
		if err != nil {
			fmt.Fprintf(out, "%s: %v\n\n", name, err)
			continue
		}
		fmt.Fprintf(out, "%s\n", m.Name)
		// Keyed by file, and the env var is deliberately absent. It is the
		// name the broker reads and the one an operator must NOT learn to
		// type — its value is a path inside a container, not the credential —
		// so listing it here would teach exactly the vocabulary the selector
		// gave up. The mapping still lives in the manifest and the .env.
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, s := range m.Secrets {
			claimed[s.File] = true
			fmt.Fprintf(w, "  %s\t%s\n", s.File, describeState(store.Stat(s.File), s))
			fmt.Fprintf(w, "  \t%s\n", s.Prompt)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(out)
	}

	// Files nothing claims. A credential no installed provider reads is still
	// mounted into every broker on this machine, which is worth reporting for
	// the same reason a forgotten lab is.
	files, err := store.Files()
	if err != nil {
		return err
	}
	var orphans []string
	for _, f := range files {
		if !claimed[f] {
			orphans = append(orphans, f)
		}
	}
	if len(orphans) > 0 {
		fmt.Fprintf(out, "claimed by no installed provider:\n")
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, f := range orphans {
			fmt.Fprintf(w, "  %s\t%s\n", f, describeState(store.Stat(f), manifest.Secret{}))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nEach of these is mounted into every broker on this machine. Delete what\nis no longer used, or install the provider that reads it.\n")
	}
	return nil
}

func describeState(st secrets.State, s manifest.Secret) string {
	if !st.Set {
		if s.Optional {
			return "not set (optional)"
		}
		return "not set"
	}
	line := fmt.Sprintf("set — %04o, modified %s", st.Mode.Perm(), st.Modified.Format("2006-01-02 15:04"))
	if st.Loose() {
		line += "  ← readable by other users; `sal secrets set` rewrites it 0600"
	}
	return line
}

// labsUsing returns the labs on this machine that installed a provider, so an
// overwrite can say how far it reaches.
func labsUsing(provider string) ([]string, error) {
	var using []string
	err := eachLab(func(l *lab.Lab, rec *deployment.Record) {
		for _, name := range rec.Names() {
			if name == provider {
				using = append(using, l.Name)
				return
			}
		}
	})
	sort.Strings(using)
	return using, err
}

// installedEverywhere is the union of what every lab on this machine has
// installed.
func installedEverywhere() ([]string, error) {
	seen := map[string]bool{}
	err := eachLab(func(_ *lab.Lab, rec *deployment.Record) {
		for _, name := range rec.Names() {
			seen[name] = true
		}
	})
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, err
}

// eachLab visits every deployment whose record can be read.
//
// A lab whose record cannot be read is SKIPPED, and that is only defensible
// because of what the two callers do with the answer: one counts how many labs
// share a credential, the other collects which providers to list. Both degrade
// to an understatement, which is survivable. `sal labs list` deliberately does
// not use this — an unreadable record is exactly what an inventory must report
// rather than skip.
func eachLab(visit func(*lab.Lab, *deployment.Record)) error {
	labs, err := lab.All()
	if err != nil {
		return err
	}
	for _, l := range labs {
		rec, err := deployment.Load(l.Dir)
		if err != nil {
			continue
		}
		visit(l, rec)
	}
	return nil
}

func sharedBy(labs []string) string {
	switch len(labs) {
	case 0, 1:
		return ""
	}
	return fmt.Sprintf(", and read by %d labs on this machine — overwriting rotates it for all of them", len(labs))
}
