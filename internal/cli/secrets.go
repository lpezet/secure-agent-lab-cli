package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
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
	var fromFile, stack string
	cmd := &cobra.Command{
		Use:   "set PROVIDER [SECRET]",
		Short: "Store a credential, read from the terminal with echo off",
		Long: "Walks the credentials the provider's manifest declares, prompting with the\n" +
			"bank author's own words. A provider declaring one credential needs no SECRET\n" +
			"argument; one declaring several takes the env name it lists, or any\n" +
			"unambiguous part of it.\n\n" +
			"The value is read from the terminal with echo off, and a non-terminal stdin is\n" +
			"refused rather than read. There is no flag to pass the value, and there will\n" +
			"not be one: an argv is in shell history, in ps, and in any process listing the\n" +
			"agent can run — and a pipe is an argv one process upstream.\n\n" +
			"--from-file is not an exception to that. A PATH in an argv reveals nothing; a\n" +
			"VALUE in an argv is the whole problem. Typing a path at the prompt works too,\n" +
			"and is confirmed before anything is copied.\n\n" +
			"Credentials are shared by every lab on this machine, so overwriting one\n" +
			"rotates it for all of them. That is stated and confirmed, never assumed.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) > 1 {
				selector = args[1]
			}
			return runSecretsSet(cmd, args[0], selector, fromFile, stack)
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read the credential from a file instead of prompting")
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

func runSecretsSet(cmd *cobra.Command, provider, selector, fromFile, stack string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

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
	if fromFile != "" && len(chosen) != 1 {
		return fmt.Errorf("--from-file sets one credential, but %s declares %d; name which one:\n%s",
			m.Name, len(chosen), secretMenu(m))
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
		value, err := readSecretValue(cmd, s, fromFile)
		if err != nil {
			return err
		}
		if len(value) == 0 {
			if !s.Optional {
				return fmt.Errorf("%s is required", s.Env)
			}
			fmt.Fprintf(errOut, "left %s unset (optional)\n", s.Env)
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

// readSecretValue gets one credential's bytes, from --from-file or the
// terminal.
func readSecretValue(cmd *cobra.Command, s manifest.Secret, fromFile string) ([]byte, error) {
	errOut := cmd.ErrOrStderr()

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

	return prompt.ReadSecret(s.Prompt, s.Multiline, fileHook(cmd, s.File))
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
// A selector matches the env name exactly, or unambiguously as a
// case-insensitive substring. Ambiguity is REFUSED rather than resolved:
// guessing wrong writes an OAuth token into the file the broker sends as
// x-api-key, and that failure is invisible until a request is rejected.
func selectSecrets(m *manifest.Manifest, selector string) ([]manifest.Secret, error) {
	if selector == "" {
		return m.Secrets, nil
	}

	var matches []manifest.Secret
	for _, s := range m.Secrets {
		if strings.EqualFold(s.Env, selector) {
			return []manifest.Secret{s}, nil
		}
		if strings.Contains(strings.ToLower(s.Env), strings.ToLower(selector)) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 1:
		return matches, nil
	case 0:
		return nil, fmt.Errorf("%s declares no credential matching %q:\n%s", m.Name, selector, secretMenu(m))
	}
	return nil, fmt.Errorf("%q matches %d of %s's credentials; name one exactly:\n%s",
		selector, len(matches), m.Name, secretMenu(m))
}

func secretMenu(m *manifest.Manifest) string {
	var b strings.Builder
	for _, s := range m.Secrets {
		fmt.Fprintf(&b, "  %s — %s\n", s.Env, s.Prompt)
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
		for _, s := range m.Secrets {
			claimed[s.File] = true
			fmt.Fprintf(out, "  %s\n    %s\n", s.Env, describeState(store.Stat(s.File), s))
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
		for _, f := range orphans {
			fmt.Fprintf(out, "  %s — %s\n", f, describeState(store.Stat(f), manifest.Secret{}))
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
	line := fmt.Sprintf("set — %s, %04o, modified %s", st.File, st.Mode.Perm(), st.Modified.Format("2006-01-02 15:04"))
	if st.Loose() {
		line += "  ← readable by other users; `sal secrets set` rewrites it 0600"
	}
	return line
}

// labsUsing returns the labs on this machine that installed a provider, so an
// overwrite can say how far it reaches.
func labsUsing(provider string) ([]string, error) {
	root, err := config.LabsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var using []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := deployment.Load(filepath.Join(root, e.Name()))
		if err != nil {
			// A lab whose record cannot be read is not a reason to refuse to
			// store a credential; it only makes the count a floor.
			continue
		}
		for _, name := range rec.Names() {
			if name == provider {
				using = append(using, e.Name())
				break
			}
		}
	}
	sort.Strings(using)
	return using, nil
}

// installedEverywhere is the union of what every lab on this machine has
// installed.
func installedEverywhere() ([]string, error) {
	root, err := config.LabsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := deployment.Load(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, name := range rec.Names() {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func sharedBy(labs []string) string {
	switch len(labs) {
	case 0, 1:
		return ""
	}
	return fmt.Sprintf(", and read by %d labs on this machine — overwriting rotates it for all of them", len(labs))
}
