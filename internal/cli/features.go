package cli

import "github.com/spf13/cobra"

// newFeaturesCmd builds the `sal features` group.
//
// Enable, disable and list are the same operation for every feature, so they
// live here rather than being copied into each feature's own group. If each
// feature owned a copy there would be no single place to answer "what is on?",
// which is the question that matters on a security tool.
//
// This mirrors gcloud, which does both: `gcloud services enable NAME` for
// lifecycle and `gcloud run deploy` for a service's own actions — and notably
// `gcloud services disable NAME`, never `gcloud run disable`.
func newFeaturesCmd() *cobra.Command {
	group := newGroup("features", "Turn optional parts of the stack on and off")
	group.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List features and whether each is enabled",
			Args:  cobra.NoArgs,
			RunE:  notImplemented,
		},
		&cobra.Command{
			Use:   "enable NAME",
			Short: "Enable a feature in this lab",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented,
		},
		&cobra.Command{
			Use:   "disable NAME",
			Short: "Disable a feature in this lab",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented,
		},
	)
	return group
}
