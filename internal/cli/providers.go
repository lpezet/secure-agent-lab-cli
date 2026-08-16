package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/bank"
	"github.com/lpezet/secure-agent-lab-cli/internal/config"
	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/envfile"
	"github.com/lpezet/secure-agent-lab-cli/internal/installer"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
	"github.com/lpezet/secure-agent-lab-cli/internal/manifest"
	"github.com/lpezet/secure-agent-lab-cli/internal/prompt"
	"github.com/lpezet/secure-agent-lab-cli/internal/scaffold"
	"github.com/lpezet/secure-agent-lab-cli/internal/secrets"
)

// newProvidersCmd builds the `sal providers` group.
//
// Everything under here is generic over the bank. `sal providers add <name>`
// must work by someone dropping bank/<name>/ into the stack repo with zero
// changes in this repo — so no command here may branch on which provider it
// was handed. internal/invariants holds the test that says so.
func newProvidersCmd() *cobra.Command {
	group := newGroup("providers", "Install and manage credential providers from the bank")
	group.AddCommand(
		newProvidersListCmd(),
		newProvidersAddCmd(),
		newProvidersCreateCmd(),
		newProvidersRemoveCmd(),
	)
	return group
}

func newProvidersListCmd() *cobra.Command {
	var (
		available bool
		stack     string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List providers installed in this lab",
		Long: "By default, what is installed here — read from .sal/installed.json rather than\n" +
			"guessed from filenames.\n\n" +
			"With --available, what the bank offers at the stack release this lab is pinned\n" +
			"to. Not at the newest release: offering entries from a bank newer than the lab\n" +
			"runs would mean offering ones whose min_stack it does not satisfy, and that\n" +
			"failure lands at runtime inside a container rather than here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if available {
				return runProvidersAvailable(cmd, stack)
			}
			return runProvidersInstalled(cmd)
		},
	}
	cmd.Flags().BoolVar(&available, "available", false, "list what the bank offers at this lab's pinned stack tag, not what is installed")
	cmd.Flags().StringVar(&stack, "stack", "", "read the bank at this release instead of the one this lab is pinned to")
	return cmd
}

func runProvidersInstalled(cmd *cobra.Command) error {
	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}

	if len(rec.Installed) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "no providers installed in %s\n", l.Dir)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSLOT\tSCHEMA\tFROM")
	for _, e := range rec.Installed {
		from := e.StackTag
		if from == "" {
			from = rec.StackTag
		}
		fmt.Fprintf(w, "%s\t%03d\t%d\t%s\n", e.Name, e.Slot, e.SchemaVersion, from)
	}
	return w.Flush()
}

func runProvidersAvailable(cmd *cobra.Command, stackOverride string) error {
	sc, err := resolveStack(cmd, stackOverride)
	if err != nil {
		return err
	}

	b, tree, err := openBank(cmd, sc.BankCommit)
	if err != nil {
		return err
	}
	defer tree.Close()
	names, err := b.List()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMIN STACK\tSUMMARY")
	for _, name := range names {
		m, err := b.Manifest(name)
		if err != nil {
			// One unreadable entry must not hide the rest of the bank, but it
			// must be visible rather than skipped silently.
			fmt.Fprintf(w, "%s\t?\t(unreadable: %v)\n", name, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s%s\n", m.Name, m.MinStack, m.Summary, installability(m, sc))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "\nbank at stack %s (%s)\n", sc.BankTag, shortCommit(sc.BankCommit))
	if sc.LabTag != "" && sc.LabTag != sc.BankTag {
		fmt.Fprintf(out, "this lab runs stack %s, so installability is judged against that\n", sc.LabTag)
	}
	if sc.LabTag == "" {
		fmt.Fprintf(out, "not in a lab, so whether each entry could be installed is unknown\n")
	}
	return nil
}

// installability annotates an entry with the reason it could not be installed
// HERE, judged against what this lab runs rather than against the release the
// entry was published in.
//
// With no lab there is nothing to judge against, and the honest answer is
// silence — a blank column beats one that always says "fine".
func installability(m *manifest.Manifest, sc stackContext) string {
	if err := m.CheckSchemaVersion(); err != nil {
		return "  [needs a newer sal]"
	}
	if sc.LabTag == "" {
		return ""
	}
	if err := m.CheckMinStack(sc.LabTag); err != nil {
		return "  [needs stack " + m.MinStack + "]"
	}
	return ""
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "commit unrecorded"
	}
	return sha
}

func newProvidersAddCmd() *cobra.Command {
	var dryRun, noEgress bool
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Install a bank entry into this lab",
		Long: "Installs the four files a bank entry may carry — the broker provider, the proxy\n" +
			"addon, the cred-gateway config and an optional lab setup fragment — and records\n" +
			"what it wrote in .sal/installed.json.\n\n" +
			"Order matters and is not negotiable:\n\n" +
			"  1. Refuse a manifest whose schema_version is above what this build supports.\n" +
			"     It may declare a control that would be silently skipped.\n" +
			"  2. Refuse an entry whose min_stack is above this lab's pin, BEFORE writing\n" +
			"     any file. That failure is silent at install and fatal at runtime.\n" +
			"  3. Assign the addon's NNN prefix: the lowest free slot in the manifest's\n" +
			"     band. The bank never bakes a number in.\n" +
			"  4. Prompt for secrets and config separately — different storage, different\n" +
			"     permissions, different prompt. A secret is a 0600 file under the secrets\n" +
			"     directory; config is a value in .env.\n\n" +
			"A route the manifest marks `exposed: false` must not appear in any generated\n" +
			"gateway config. Exposing a token route would hand the lab a reusable secret.\n\n" +
			"From stack 1.13.0 an entry also declares the egress it needs, and the lines it\n" +
			"left uncommented are added to this lab's allowlist and printed. Only those: a\n" +
			"commented line is a suggestion, and turning it on is yours to type. --no-egress\n" +
			"installs the credential path and grants nothing, for staging one before the\n" +
			"other.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvidersAdd(cmd, args[0], dryRun, noEgress)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run every check and print what would be written, without writing it")
	cmd.Flags().BoolVar(&noEgress, "no-egress", false, "install the credential path without permitting the entry's destinations")
	return cmd
}

func runProvidersAdd(cmd *cobra.Command, name string, dryRun, noEgress bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	if !l.Exists() {
		return fmt.Errorf("lab %q has no deployment at %s; run `sal init`", l.Name, l.Dir)
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}
	// Which addon numbers are already taken — from the record and from disk,
	// since a stray addon the record does not mention must not have its number
	// reused. Read into a set, never folded into the record: the record says
	// which BANK ENTRIES are installed, and a stack addon is not one.
	occupied := installer.OccupiedSlots(l.Dir, rec)

	// The commit this lab was built from, straight out of its own record —
	// no tag resolution, and no way for a moved tag to change what an
	// existing lab installs from.
	commit, err := commitFor(cmd, rec)
	if err != nil {
		return err
	}
	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return err
	}
	defer tree.Close()

	// An entry the operator wrote themselves is installed from their own
	// providers directory. Which one this is has to be decided here, because
	// it is also what gets recorded — the two are indistinguishable afterwards
	// and they are not equivalent.
	source, b, err := resolveSource(b, name)
	if err != nil {
		return err
	}
	if source == deployment.SourceLocal {
		fmt.Fprintf(errOut, "installing %s from your own providers directory, not from the bank at %s.\n"+
			"Nobody has reviewed it but you: every check sal has still runs, and none of\n"+
			"them can tell whether the broker provider hands the lab more than it should.\n\n",
			name, rec.StackTag)
	}

	// Everything that can be refused is refused here, before a byte is
	// written. A half-installed credential path is worse than none.
	plan, err := installer.BuildPlan(b, name, rec, occupied, rec.StackTag)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s — %s\n", plan.Manifest.Name, plan.Manifest.Summary)
	fmt.Fprintf(out, "slot     %03d (%s band)\n", plan.Slot, plan.Manifest.LoadBand)
	fmt.Fprintf(out, "hosts    %s\n", strings.Join(plan.Manifest.Hosts, ", "))
	for _, f := range plan.Files {
		fmt.Fprintf(out, "write    %s\n", f.Dst)
	}
	for _, r := range plan.Manifest.BrokerRoutes {
		state := "not exposed to the lab"
		if r.IsExposed() {
			state = "whitelisted for the lab"
		}
		fmt.Fprintf(out, "route    %s — %s\n", r.Path, state)
	}
	// Egress is reported with the rest of the plan rather than after the fact,
	// because it is the one line here that WIDENS the boundary — a --dry-run
	// that showed everything except what it would permit would be describing
	// the wrong half.
	for _, l := range plan.Egress.Enabled {
		fmt.Fprintf(out, "allow    %s\n", l.Text)
	}

	if dryRun {
		fmt.Fprintf(errOut, "\ndry run: every check passed and nothing was written\n")
		return nil
	}

	secretsDir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	values, err := collectValues(cmd, plan, secretsDir)
	if err != nil {
		return err
	}

	entry, err := plan.Apply(l.Dir, secretsDir, rec.StackTag, values)
	if err != nil {
		return err
	}
	entry.Source = source
	rec.Installed = append(rec.Installed, *entry)
	if err := deployment.Save(l.Dir, rec); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "\ninstalled %s into %s\n", entry.Name, l.Name)

	// Seeding the allowlist widens egress, so what was granted is stated here
	// rather than left to be discovered. It is done AFTER the record is saved:
	// the entry owning a block is what lets `providers remove` close the grant
	// again, so a block written for an entry no record names would be a
	// permission nothing knows how to take back.
	if err := seedEgress(cmd, l.Dir, entry.Name, plan.Egress, noEgress); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "Run `sal up` to restart the lab against it — the broker, proxy and\n"+
		"cred-gateway read these files at startup, so a running lab has not picked them up.\n")

	// Honest about a gap rather than quietly leaving a file nothing reads.
	for _, f := range plan.Files {
		if strings.HasPrefix(f.Dst, "lab/") {
			fmt.Fprintf(errOut, "\nnote: %s was installed but is NOT sourced automatically. The stack's\n"+
				"      lab_setup mechanism assumes the deployment sits inside the workspace, and\n"+
				"      here it deliberately does not. Run it yourself with:\n"+
				"        docker compose -f %s exec lab bash /workspace/../%s\n",
				f.Dst, l.ComposeFile(), f.Dst)
		}
	}
	return nil
}

// collectValues prompts for what the manifest declares, keeping the two kinds
// apart: a secret is a file under the secrets directory read with echo off, a
// config value is a line in .env. Conflating them is how a credential ends up
// somewhere it can be read.
func collectValues(cmd *cobra.Command, plan *installer.Plan, secretsDir string) (installer.Values, error) {
	v := installer.Values{
		Secrets: map[string][]byte{},
		Config:  map[string]string{},
	}
	errOut := cmd.ErrOrStderr()

	for _, s := range plan.Manifest.Secrets {
		path := filepath.Join(secretsDir, s.File)
		if _, err := os.Stat(path); err == nil {
			// Credentials are shared across labs on this machine. Re-prompting
			// for one already stored invites pasting a different value and
			// silently repointing every other lab that uses it.
			fmt.Fprintf(errOut, "using the %s already stored at %s\n", s.File, secretsDir)
			// Reuse skips the write, so it would also skip the mode. A
			// credential that arrived at 0644 by some other route must not
			// stay that way just because sal did not create it.
			tightened, err := secrets.EnsureMode(path)
			if err != nil {
				return v, err
			}
			if tightened {
				fmt.Fprintf(errOut, "  tightened %s to 0600\n", s.File)
			}
			continue
		}

		// Same prompt as `sal secrets set`, including the option to give a path
		// instead of pasting. Installing a provider is where most credentials
		// arrive, so it would be the wrong place to make the operator paste a
		// PEM by hand.
		value, err := prompt.ReadSecret(s.Prompt, s.File, s.Multiline, fileHook(cmd, s.File))
		if err != nil {
			return v, err
		}
		if len(value) == 0 {
			if s.Optional {
				fmt.Fprintf(errOut, "skipped %s (optional)\n", s.File)
				continue
			}
			return v, fmt.Errorf("%s is required", s.File)
		}
		v.Secrets[s.Env] = secrets.Normalize(value, s.Multiline)
	}

	for _, c := range plan.Manifest.Config {
		if c.Help != "" {
			fmt.Fprintf(errOut, "  %s\n", c.Help)
		}
		value, err := prompt.Line(c.Prompt, c.Default)
		if err != nil {
			return v, err
		}
		v.Config[c.Env] = value
	}
	return v, nil
}

func newProvidersCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create NAME",
		Short: "Scaffold a new provider you can install into this lab",
		Long: "Scaffolds a bank entry for a provider the bank does not carry, in your own\n" +
			"providers directory. `sal providers add NAME` then installs it from there.\n\n" +
			"It goes OUTSIDE the project, next to your labs and secrets. An entry is code\n" +
			"that runs behind the credential boundary once installed, so a scaffold in the\n" +
			"workspace would be one the agent could edit before you installed it.\n\n" +
			"The layout is the bank's own, so what you write here can be proposed to the\n" +
			"bank unchanged — and sal reads it with the same code that reads the bank.\n\n" +
			"There is no --template yet. A flag with one legal value is a promise about a\n" +
			"naming scheme nobody has designed; templates will arrive as shapes emerge from\n" +
			"actually writing providers.\n\n" +
			"Note the boundary: the generation constraints for writing a provider from\n" +
			"scratch live in the stack repo's PLAYBOOK.md, which covers exactly the case a\n" +
			"bank of finished entries cannot. This command scaffolds; it does not replace\n" +
			"reading that.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvidersCreate(cmd, args[0])
		},
	}
}

func runProvidersCreate(cmd *cobra.Command, name string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	root, err := config.ProvidersDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, name)

	// Refused rather than shadowed, and checked BEFORE writing anything. Two
	// entries of the same name in two banks is a question sal has no way to
	// answer — and answering it silently, in either direction, is how someone
	// installs something other than the reviewed entry they asked for.
	if taken, where := nameTakenInBank(cmd, name); taken {
		return fmt.Errorf("the bank at %s already has an entry named %q.\n"+
			"Two entries with one name is ambiguous at install time, so pick another name — "+
			"or `sal providers add %s` if that entry is the one you wanted", where, name, name)
	}

	files, err := scaffold.Write(dir, name)
	if err != nil {
		var exists *scaffold.ErrExists
		if errors.As(err, &exists) {
			return fmt.Errorf("%s already exists; delete it or pick another name rather than "+
				"having sal write over work you may have done there", exists.Dir)
		}
		return err
	}

	fmt.Fprintf(out, "%s\n", dir)
	for _, f := range files {
		fmt.Fprintf(out, "write    %s\n", f.Path)
	}

	fmt.Fprintf(errOut, "\nA skeleton, not a provider. Three things it needs before it does anything:\n"+
		"  1. hosts in %s must match the addon exactly, both directions.\n"+
		"  2. The broker provider must exchange the long-lived credential for something\n"+
		"     scoped and short-lived, and never log a value.\n"+
		"  3. Only routes marked exposed:true may appear in the cred-gateway config.\n\n"+
		"Read PLAYBOOK.md in the stack repo — it covers writing one from scratch, which\n"+
		"is what you are about to do. Then `sal providers add %s --dry-run` runs every\n"+
		"check sal has against it without writing anything.\n", manifest.Filename, name)
	return nil
}

// resolveSource decides which bank an entry name comes from, and refuses a
// name that is in both.
//
// Refusing is the only answer sal can give honestly. Preferring the local copy
// silently installs something other than the reviewed entry somebody asked
// for; preferring the bank silently ignores the one they wrote. Both are the
// wrong shape of surprise for a command that installs code behind a credential
// boundary — and the refusal is what a naming scheme will eventually replace,
// rather than something it will have to undo.
func resolveSource(remote *bank.Bank, name string) (string, *bank.Bank, error) {
	dir, err := config.ProvidersDir()
	if err != nil {
		return "", remote, err
	}
	local, err := bank.OpenDir(dir)
	if err != nil {
		return deployment.SourceBank, remote, nil
	}
	if _, err := local.EntryDir(name); err != nil {
		return deployment.SourceBank, remote, nil
	}
	if _, err := remote.EntryDir(name); err == nil {
		return "", remote, fmt.Errorf(
			"%q names both an entry in the bank and one in %s.\n"+
				"sal will not guess which you meant — rename yours, or remove it and use the "+
				"bank's", name, filepath.Join(dir, name))
	}
	return deployment.SourceLocal, local, nil
}

// nameTakenInBank reports whether the bank this lab reads already has the name.
//
// Best-effort on purpose: not being in a lab, or not being able to reach the
// bank, must not stop someone scaffolding a provider. What it prevents when it
// does work is a collision discovered at install time, after the writing.
func nameTakenInBank(cmd *cobra.Command, name string) (bool, string) {
	l, _, err := lab.Find(cwd())
	if err != nil {
		return false, ""
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return false, ""
	}
	commit, err := commitFor(cmd, rec)
	if err != nil {
		return false, ""
	}
	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return false, ""
	}
	defer tree.Close()

	if _, err := b.EntryDir(name); err != nil {
		return false, ""
	}
	return true, rec.StackTag
}

func newProvidersRemoveCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an installed provider and its files",
		Long: "Removes exactly the files .sal/installed.json records for the entry, rather\n" +
			"than guessing from filenames. Removing a provider narrows the boundary, so it\n" +
			"is safe to get slightly wrong in the cautious direction and dangerous to get\n" +
			"wrong in the other: leave anything unrecorded in place and say so.\n\n" +
			"The cred-gateway config is the file that matters most here. Left behind, it\n" +
			"keeps whitelisting a route to a broker provider that is gone — a widened\n" +
			"boundary nothing else would report.\n\n" +
			"Credentials are NOT deleted. Removing a provider is reversible by reinstalling\n" +
			"it; deleting a credential is not, and the two are different decisions. Which\n" +
			"files were left is printed, so `rm` is one line away if that is what you meant.\n\n" +
			"No confirmation prompt: this narrows the boundary, and a prompt on a safe\n" +
			"action only teaches people to clear prompts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvidersRemove(cmd, args[0], dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed, and remove nothing")
	return cmd
}

func runProvidersRemove(cmd *cobra.Command, name string, dryRun bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, _, err := lab.Find(cwd())
	if err != nil {
		return err
	}
	if !l.Exists() {
		return fmt.Errorf("lab %q has no deployment at %s; run `sal init`", l.Name, l.Dir)
	}
	rec, err := deployment.Load(l.Dir)
	if err != nil {
		return err
	}

	// Exact match, for the reason a lab name is: two entries whose names share
	// a prefix are exactly where a guess does damage, and the damage here is
	// deleting the files of a credential path somebody is using.
	entry, rest, found := takeEntry(rec.Installed, name)
	if !found {
		return fmt.Errorf("%q is not installed in lab %q.\n%s", name, l.Name, installedMenu(rec))
	}

	fmt.Fprintf(out, "%s — slot %03d\n", entry.Name, entry.Slot)
	for _, f := range entry.Files {
		fmt.Fprintf(out, "delete   %s\n", f)
	}

	// Read for its env keys only. An entry the bank no longer carries still
	// gets its recorded files removed — those are recorded, not derived — and
	// the keys are reported as left behind rather than guessed at.
	m := manifestFor(cmd, rec, entry.Name)
	if m != nil {
		for _, k := range envKeysOf(m) {
			fmt.Fprintf(out, "unset    %s\n", k)
		}
	}

	// Reported with the files, because closing egress is part of what removal
	// means — and a --dry-run that listed the files without it would describe
	// a smaller change than the one about to happen.
	if err := dropEgress(cmd, l.Dir, entry.Name, true); err != nil {
		return err
	}

	if dryRun {
		fmt.Fprintf(errOut, "\ndry run: nothing was removed\n")
		return nil
	}

	for _, f := range entry.Files {
		path := filepath.Join(l.Dir, filepath.FromSlash(f))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	if m != nil {
		if err := envfile.Remove(filepath.Join(l.Dir, envFileName), envKeysOf(m)); err != nil {
			return err
		}
		if err := envfile.Remove(filepath.Join(l.Dir, labEnvFileName), sortedKeys(m.LabEnv)); err != nil {
			return err
		}
	}

	// Before the record is saved, which is the reverse of the install order
	// and points the same way: the state that must not be reachable is a
	// deployment whose record no longer names the entry while its destinations
	// are still permitted. Nothing would report that grant, because nothing
	// would know to look for it.
	if err := dropEgress(cmd, l.Dir, entry.Name, false); err != nil {
		return err
	}

	rec.Installed = rest
	if err := deployment.Save(l.Dir, rec); err != nil {
		return err
	}

	fmt.Fprintf(errOut, "\nremoved %s from %s\n", entry.Name, l.Name)
	if m == nil {
		fmt.Fprintf(errOut, "warning: %s could not be read from the bank at %s, so any variables it\n"+
			"         declared are still in %s. Nothing reads them now, but they describe a\n"+
			"         credential path that no longer exists.\n", entry.Name, rec.StackTag, envFileName)
	}
	reportLeftBehind(cmd, l.Dir, entry, rec)
	reportKeptSecrets(cmd, m)
	fmt.Fprintf(errOut, "\nRun `sal up` to restart the lab without it — the broker, proxy and\n"+
		"cred-gateway read these files at startup, so a running lab is still serving\n"+
		"the routes this removed.\n")
	return nil
}

// takeEntry returns the named entry and the others.
func takeEntry(entries []deployment.Entry, name string) (deployment.Entry, []deployment.Entry, bool) {
	var (
		found deployment.Entry
		rest  []deployment.Entry
		ok    bool
	)
	for _, e := range entries {
		if e.Name == name {
			found, ok = e, true
			continue
		}
		rest = append(rest, e)
	}
	if rest == nil {
		rest = []deployment.Entry{}
	}
	return found, rest, ok
}

func installedMenu(rec *deployment.Record) string {
	names := rec.Names()
	if len(names) == 0 {
		return "This lab has no providers installed."
	}
	return "It has: " + strings.Join(names, ", ")
}

// manifestFor reads an entry's manifest from the bank at the pin, or nil.
//
// Nil is a normal answer, not a failure: an entry can be removed from the bank
// between installing it and removing it, and a deployment that cannot reach the
// network still needs to be able to take a provider out. Whatever this cannot
// answer is REPORTED rather than guessed — the alternative is deriving env keys
// from a provider name, which is the per-provider knowledge this repo has none
// of.
func manifestFor(cmd *cobra.Command, rec *deployment.Record, name string) *manifest.Manifest {
	commit, err := commitFor(cmd, rec)
	if err != nil {
		return nil
	}
	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return nil
	}
	defer tree.Close()

	m, err := b.Manifest(name)
	if err != nil {
		return nil
	}
	return m
}

// envKeysOf returns the .env keys an entry owns: the path variables its
// secrets are read through, and its configuration. Not lab.env, which is a
// different file for a different container.
func envKeysOf(m *manifest.Manifest) []string {
	var keys []string
	for _, s := range m.Secrets {
		keys = append(keys, s.Env)
	}
	for _, c := range m.Config {
		keys = append(keys, c.Env)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// reportLeftBehind names files that look like this entry's and that nothing
// recorded, which are therefore still in place.
//
// The cautious direction, and it is not symmetric: deleting an unrecorded file
// because its name matches is how a provider removal takes out something
// somebody wrote by hand, while leaving one costs a line of output. A
// cred-gateway config in this state is worth chasing, because it keeps
// whitelisting a route whose broker provider has just been deleted.
func reportLeftBehind(cmd *cobra.Command, deployDir string, entry deployment.Entry, rec *deployment.Record) {
	recorded := map[string]bool{}
	for _, e := range rec.Installed {
		for _, f := range e.Files {
			recorded[filepath.ToSlash(f)] = true
		}
	}

	var left []string
	for _, dir := range []string{"broker", "proxy", "cred-gateway", "lab"} {
		items, err := os.ReadDir(filepath.Join(deployDir, dir))
		if err != nil {
			continue
		}
		for _, it := range items {
			if it.IsDir() || !strings.Contains(it.Name(), entry.Name) {
				continue
			}
			rel := dir + "/" + it.Name()
			if !recorded[rel] {
				left = append(left, rel)
			}
		}
	}
	if len(left) == 0 {
		return
	}

	sort.Strings(left)
	fmt.Fprintf(cmd.ErrOrStderr(), "\nwarning: these look like %s and nothing recorded them, so they are still\n"+
		"         here: %s\n"+
		"         Check them by hand. A cred-gateway config left behind keeps whitelisting\n"+
		"         a route whose provider is gone.\n", entry.Name, strings.Join(left, ", "))
}

// reportKeptSecrets names the credential files that stayed.
func reportKeptSecrets(cmd *cobra.Command, m *manifest.Manifest) {
	if m == nil || len(m.Secrets) == 0 {
		return
	}
	secretsDir, err := config.SecretsDir()
	if err != nil {
		return
	}

	var kept []string
	for _, s := range m.Secrets {
		path := filepath.Join(secretsDir, s.File)
		if _, err := os.Stat(path); err == nil {
			kept = append(kept, path)
		}
	}
	if len(kept) == 0 {
		return
	}

	sort.Strings(kept)
	fmt.Fprintf(cmd.ErrOrStderr(), "\nIts credentials were kept, because deleting one is not reversible and is a\n"+
		"different decision from removing a provider:\n")
	for _, k := range kept {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", k)
	}
}
