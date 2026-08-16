package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lpezet/secure-agent-lab-cli/internal/egress"
)

// seedEgress adds an entry's declared destinations to this lab's allowlist and
// says what it granted.
//
// Seeding by default is defensible on one specific ground, and it is worth
// being able to state: the entry's broker provider and proxy addon already run
// behind the credential boundary, so an operator who installed it has already
// extended it more trust than "you may reach the host you say you need".
// Permitting that host is not a new grant so much as the one implied by the
// install — and refusing to do it produced the failure this replaces, where a
// provider installs cleanly and every request it makes is denied.
//
// What makes that safe is the line it does NOT cross: only what the entry left
// uncommented. A commented destination is the vendor's suggestion, not the
// entry's requirement, and it stays denied until an operator types it.
//
// Saying what was granted is not politeness. This is the one part of an install
// that widens the boundary, and a widening nobody is told about is the kind
// that is still there in six months because nobody knew to question it.
func seedEgress(cmd *cobra.Command, labDir, name string, e egress.Entry, skip bool) error {
	errOut := cmd.ErrOrStderr()
	path := filepath.Join(labDir, allowlistName)

	if skip {
		if len(e.Enabled) > 0 {
			fmt.Fprintf(errOut, "\negress   NOT granted (--no-egress). %s needs %d destination(s),\n"+
				"         and every request to them is denied until you add them to\n         %s:\n",
				name, len(e.Enabled), path)
			for _, l := range e.Enabled {
				fmt.Fprintf(errOut, "           %s\n", l.Text)
			}
		}
		return nil
	}

	// An entry from a release older than 1.13.0 ships no allowlist. Nothing is
	// granted and nothing is claimed — but the operator still has to permit
	// the hosts by hand, so this is where they are told, rather than leaving
	// them to infer it from a denial at runtime.
	if len(e.Enabled) == 0 {
		fmt.Fprintf(errOut, "\negress   this entry declares none, so nothing was permitted.\n"+
			"         If its requests are denied, add what it needs to %s —\n"+
			"         one per line, `domain METHODS`. Omitting METHODS means GET,HEAD,OPTIONS.\n", path)
		return nil
	}

	granted, err := egress.Write(path, name, e.Enabled)
	if err != nil {
		return fmt.Errorf("installed %s, but could not grant it egress: %w", name, err)
	}

	fmt.Fprintf(errOut, "\negress   permitted %d destination(s) in %s:\n", len(granted), path)
	for _, l := range granted {
		fmt.Fprintf(errOut, "           %s\n", egress.Describe(l))
	}
	if len(e.Optional) > 0 {
		// The other half of understanding your egress: what this entry offers
		// and you are not taking. Printed because the question an operator
		// asks later is "why can it not reach X", and the answer is often
		// sitting commented out in this very file.
		fmt.Fprintf(errOut, "         %d more the entry offers and did NOT enable:\n", len(e.Optional))
		for _, l := range e.Optional {
			fmt.Fprintf(errOut, "           %s\n", egress.Describe(l))
		}
		fmt.Fprintf(errOut, "         Uncomment them there if you want them.\n")
	}
	return nil
}

// dropEgress closes what an entry was granted, and is the reason the grant is
// written inside a marked block at all.
//
// Same asymmetry as the file removal it accompanies: what the block says is
// removed, and anything the operator wrote outside it is left alone and
// untouched. A destination left permitted after its provider is gone is the
// widened boundary nothing else would report — the egress twin of a stale
// cred-gateway config.
func dropEgress(cmd *cobra.Command, labDir, name string, dryRun bool) error {
	path := filepath.Join(labDir, allowlistName)

	if dryRun {
		owned, err := egress.Blocks(path)
		if err != nil {
			return err
		}
		for _, l := range owned[name] {
			fmt.Fprintf(cmd.OutOrStdout(), "revoke   %s\n", l.Text)
		}
		return nil
	}

	removed, err := egress.Remove(path, name)
	if err != nil {
		return err
	}
	for _, l := range removed {
		fmt.Fprintf(cmd.OutOrStdout(), "revoked  %s\n", l.Text)
	}
	return nil
}
