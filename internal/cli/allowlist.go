package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/deployment"
	"github.com/lpezet/secure-agent-lab-cli/internal/egress"
	"github.com/lpezet/secure-agent-lab-cli/internal/installer"
	"github.com/lpezet/secure-agent-lab-cli/internal/lab"
)

// newAllowlistCmd builds the `sal allowlist` group.
//
// The verbs are on the FILE, not on the provider, and that is not arbitrary: a
// hand-added line belongs to no entry, so `sal providers egress` could never
// describe half of what is in there. (`sal providers config` was the other
// candidate and is ambiguous besides — the manifest already has a `config`
// field, meaning values that go into `.env`.)
//
// This is a group of four rather than one command because the questions are
// different. `list` answers "what can this lab reach, and who decided each
// line" — unanswerable by eye once there are three entries and a few
// hand-written lines, since the file gives no clue which is which. `reset`
// answers "put it back the way the entries say", which is the one nothing else
// can do.
func newAllowlistCmd() *cobra.Command {
	group := newGroup("allowlist", "See and change what this lab is permitted to reach")
	group.AddCommand(newAllowlistListCmd(), newAllowlistResetCmd(), newAllowlistAllowCmd(), newAllowlistDenyCmd())
	return group
}

func newAllowlistListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show what this lab may reach, and which entry permitted each line",
		Long: "Every destination the lab is permitted, grouped by who decided it.\n\n" +
			"A line inside a marked block came from a bank entry and is rewritten whenever\n" +
			"that entry is installed or upgraded. Everything else you wrote, and nothing sal\n" +
			"does will touch it.\n\n" +
			"This reads the file, not the running proxy. A lab that has not been restarted\n" +
			"since the file changed is still enforcing the old one — `sal up` is what makes\n" +
			"them the same.",
		Args: cobra.NoArgs,
		RunE: runAllowlistList,
	}
}

func runAllowlistList(cmd *cobra.Command, _ []string) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	l, path, err := allowlistPath()
	if err != nil {
		return err
	}

	owned, err := egress.Blocks(path)
	if err != nil {
		return err
	}
	mine, err := egress.Unmanaged(path)
	if err != nil {
		return err
	}

	if len(owned) == 0 && len(mine) == 0 {
		// Said as a finding rather than printed as an empty list. An empty
		// allowlist is ENFORCING and denies everything, which is the opposite
		// of what an empty listing usually implies.
		fmt.Fprintf(errOut, "lab %s permits nothing: %s has no destinations in it.\n"+
			"That file being present is what makes the allowlist enforcing, so every\n"+
			"request the lab makes is denied. `sal providers add <name>` installs an\n"+
			"entry's own destinations; `sal allowlist allow HOST METHODS` adds your own.\n",
			l.Name, path)
		return nil
	}

	names := make([]string, 0, len(owned))
	for name := range owned {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fmt.Fprintf(out, "%s\n", name)
		for _, line := range owned[name] {
			fmt.Fprintf(out, "  %s\n", line.Text)
		}
	}
	if len(mine) > 0 {
		fmt.Fprintf(out, "yours\n")
		for _, line := range mine {
			fmt.Fprintf(out, "  %s\n", line.Text)
		}
	}

	fmt.Fprintf(errOut, "\n%s\n", path)
	if len(names) > 0 {
		fmt.Fprintf(errOut, "Lines under an entry's name are rewritten by `sal providers add`, `sal upgrade`\n"+
			"and `sal allowlist reset`. Lines under `yours` are never touched by sal.\n")
	}
	return nil
}

func newAllowlistResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset [NAME]",
		Short: "Restore what the installed entries declare, leaving your own lines alone",
		Long: "The command for a lab that has been edited into a state nobody can explain —\n" +
			"where the symptom is an agent reporting it cannot reach its vendor, and the\n" +
			"cause is three edits ago.\n\n" +
			"Each installed entry declares the destinations it needs, so what its block\n" +
			"should contain is answerable exactly rather than remembered. This rewrites\n" +
			"those blocks from the entries and touches nothing else — a line you added\n" +
			"yourself survives, and so does a destination you turned on by uncommenting one\n" +
			"the entry ships as optional... unless you uncommented it INSIDE the block, in\n" +
			"which case it is inside something sal owns and goes.\n\n" +
			"With no NAME it resets every installed entry. Note this is not `sal drift`:\n" +
			"drift asks whether the lab is what the release ships, and deliberately does not\n" +
			"compare the allowlist at all, because that file is yours.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runAllowlistReset(cmd, name)
		},
	}
}

func runAllowlistReset(cmd *cobra.Command, only string) error {
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
	if len(rec.Installed) == 0 {
		return fmt.Errorf("lab %q has no providers installed, so there is nothing to restore.\n"+
			"Every line in its allowlist is yours", l.Name)
	}

	commit := rec.StackCommit
	b, tree, err := openBank(cmd, commit)
	if err != nil {
		return err
	}
	defer tree.Close()

	found := false
	for _, entry := range rec.Installed {
		if only != "" && entry.Name != only {
			continue
		}
		found = true

		// Read through the same path an install takes, so what reset writes
		// and what `providers add` writes cannot disagree.
		src, eb, err := resolveSource(b, entry.Name)
		if err != nil {
			return fmt.Errorf("cannot read %s to restore what it declares: %w", entry.Name, err)
		}
		_ = src
		plan, err := installer.BuildPlanAt(eb, entry.Name, entry.Slot, rec.StackTag)
		if err != nil {
			return fmt.Errorf("cannot read %s to restore what it declares: %w", entry.Name, err)
		}

		before, err := egress.Blocks(filepath.Join(l.Dir, allowlistName))
		if err != nil {
			return err
		}
		written, err := egress.Write(filepath.Join(l.Dir, allowlistName), entry.Name, plan.Egress.Enabled)
		if err != nil {
			return err
		}

		if sameLines(before[entry.Name], written) {
			fmt.Fprintf(out, "%s\n  unchanged\n", entry.Name)
			continue
		}
		fmt.Fprintf(out, "%s\n", entry.Name)
		for _, line := range written {
			fmt.Fprintf(out, "  %s\n", line.Text)
		}
	}

	if !found {
		return fmt.Errorf("%q is not installed in lab %q.\n%s", only, l.Name, installedMenu(rec))
	}

	fmt.Fprintf(errOut, "\nRun `sal up` to restart the lab against it — the proxy reads the allowlist at\n"+
		"startup, so a running lab is still enforcing what was there before.\n")
	return nil
}

func sameLines(a, b []egress.Line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

func newAllowlistAllowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "allow HOST [METHODS]",
		Short: "Permit a destination of your own",
		Long: "Adds a destination outside every managed block, which is what makes it yours:\n" +
			"it survives `sal providers remove`, and no upgrade rewrites it.\n\n" +
			"METHODS is a comma-separated list, or `*`. OMITTING IT IS NOT NEUTRAL — the\n" +
			"proxy defaults a line with no methods to GET,HEAD,OPTIONS, safe reads only, so\n" +
			"a bare host looks permitted and denies every write. sal states what it wrote\n" +
			"for that reason.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			methods := ""
			if len(args) == 2 {
				methods = args[1]
			}
			return runAllowlistAllow(cmd, args[0], methods)
		},
	}
}

func runAllowlistAllow(cmd *cobra.Command, host, methods string) error {
	out, errOut := cmd.ErrOrStderr(), cmd.ErrOrStderr()
	_ = out

	l, path, err := allowlistPath()
	if err != nil {
		return err
	}

	added, err := egress.Allow(path, host, methods)
	if err != nil {
		return err
	}
	if !added {
		fmt.Fprintf(errOut, "%s is already permitted in %s; nothing to do\n", host, l.Name)
		return nil
	}

	if methods == "" {
		// Not a warning about a mistake — it may be what was meant. It is a
		// statement of what the line now does, because the default is the one
		// thing about this file's syntax that is not visible in it.
		fmt.Fprintf(errOut, "permitted %s with no methods, which the proxy reads as GET,HEAD,OPTIONS —\n"+
			"safe reads only. `sal allowlist allow %s POST` if it needs to write.\n", host, host)
	} else {
		fmt.Fprintf(errOut, "permitted %s %s\n", host, methods)
	}
	fmt.Fprintf(errOut, "Run `sal up` to restart the lab against it — the proxy reads the allowlist at\n"+
		"startup.\n")
	return nil
}

func newAllowlistDenyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deny HOST",
		Short: "Remove one of your own destinations",
		Long: "Removes a destination you added. It refuses one that belongs to an installed\n" +
			"entry rather than deleting it: that would work until the next `sal providers\n" +
			"add`, `sal upgrade` or `sal allowlist reset` put it back, and a grant that\n" +
			"reappears with nothing to explain it is worse than one that was never removed.\n" +
			"`sal providers remove NAME` is the honest way to close that one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAllowlistDeny(cmd, args[0])
		},
	}
}

func runAllowlistDeny(cmd *cobra.Command, host string) error {
	errOut := cmd.ErrOrStderr()

	l, path, err := allowlistPath()
	if err != nil {
		return err
	}

	removed, err := egress.Deny(path, host)
	if err != nil {
		var managed *egress.ErrManaged
		if errors.As(err, &managed) {
			return fmt.Errorf("%s is permitted by the %s entry, not by you.\n"+
				"Removing it here would last until the next `sal providers add`, `sal upgrade`\n"+
				"or `sal allowlist reset` wrote that entry's block again. To close it for good,\n"+
				"`sal providers remove %s` — or edit %s if you mean to diverge from what the\n"+
				"entry declares, and expect a reset to undo that",
				host, managed.Owner, managed.Owner, path)
		}
		return err
	}
	if !removed {
		return fmt.Errorf("%s is not permitted in lab %q, so there is nothing to remove.\n"+
			"`sal allowlist list` shows what is", host, l.Name)
	}

	fmt.Fprintf(errOut, "denied %s\n", host)
	fmt.Fprintf(errOut, "Run `sal up` to restart the lab against it — the proxy reads the allowlist at\n"+
		"startup, so a running lab is still permitting it.\n")
	return nil
}

// allowlistPath resolves the lab here and the file within it.
func allowlistPath() (*lab.Lab, string, error) {
	l, _, err := lab.Find(cwd())
	if err != nil {
		return nil, "", err
	}
	if !l.Exists() {
		return nil, "", fmt.Errorf("lab %q has no deployment at %s; run `sal init`", l.Name, l.Dir)
	}
	return l, filepath.Join(l.Dir, allowlistName), nil
}
