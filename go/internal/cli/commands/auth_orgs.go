//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// collectOrgsV2 probes every credentialed session on the v2 organization
// surface (dev serves v2 only) and returns the deduplicated orgs, the first
// session that answered (used by the picker to scope its default), and the
// number of sessions that answered successfully.
//
// Every per-session failure is surfaced verbatim on w and recorded in failed —
// never swallowed. Swallowing a failed probe (the old `continue`) made a
// transport error, 401, PERMISSION_DENIED or UNIMPLEMENTED render identically
// to a genuinely empty membership, which manufactured days of false
// "adoption broken" evidence (WDY-3101). Callers distinguish "no memberships"
// (okSessions > 0, no orgs) from "every probe failed" (okSessions == 0) so the
// reassuring no-orgs sentence is never printed on a failed call.
func collectOrgsV2(ctx context.Context, cfg *config.Config, w io.Writer) (orgs []*pb.Organization, pickerAuth *config.AuthConfig, okSessions int, failed []error) {
	seen := make(map[string]bool)
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if len(a.Certificates) == 0 {
			continue
		}
		fetched, fetchErr := listCloudOrganizationsV2(ctx, a)
		if fetchErr != nil {
			e := fmt.Errorf("listing organizations for %s: %w", authSessionLabel(a), fetchErr)
			fmt.Fprintln(w, e)
			failed = append(failed, e)
			continue
		}
		okSessions++
		if pickerAuth == nil {
			pickerAuth = a
		}
		for _, org := range fetched {
			if org == nil || seen[org.GetId()] {
				continue
			}
			seen[org.GetId()] = true
			orgs = append(orgs, org)
		}
	}
	return orgs, pickerAuth, okSessions, failed
}

func newAuthListOrgsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-orgs",
		Short: "List and select your Wendy Cloud organizations",
		Long: `Show all organizations your account belongs to and optionally set a default.

With --json or outside an interactive terminal, print a JSON array of organization
IDs and names without opening the management picker or changing your default.

Press 'd' on a highlighted organization to mark it as the default for commands
that target a specific org (such as 'wendy os install --pre-enroll' and
'wendy device enroll'). Press 'x' to clear the default. Press 'r' to remove
the stored credentials for the highlighted org. Enter copies the highlighted
org's ID to the clipboard; press 'q' to quit.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if len(cfg.Auth) == 0 {
				return config.ErrNotLoggedIn
			}

			orgs, pickerAuth, okSessions, failed := collectOrgsV2(cmd.Context(), cfg, cmd.ErrOrStderr())

			if len(orgs) == 0 && okSessions == 0 {
				// Only claim an empty membership when a session actually
				// answered. If every probe failed, propagate the failure so the
				// user sees why — never the misleading no-orgs sentence.
				return errors.Join(failed...)
			}

			if jsonOutput || !isInteractiveTerminal() {
				// Keep the CLI output independent of the protobuf representation.
				// An empty membership is [] rather than null or a human message.
				type organization struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				result := make([]organization, 0, len(orgs))
				for _, org := range orgs {
					result = append(result, organization{ID: org.GetId(), Name: org.GetName()})
				}
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}

			if len(orgs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Your account belongs to no organizations.")
				return nil
			}

			// Management view: 'd'/'x'/'r' manage the default and stored
			// credentials, and Enter copies the highlighted org ID and keeps the
			// picker open. Quitting with 'q' returns ErrUserCancelled, which the
			// root silences — a clean exit for an inspect-and-manage command.
			id, err := pickCloudOrgV2(orgs, pickerAuth, cfg, true)
			if err != nil {
				return err
			}
			name := id
			for _, o := range orgs {
				if o.GetId() == id {
					name = o.GetName()
					break
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Selected organization: %s (ID: %s)\n", name, id)
			return nil
		},
	}
	return cmd
}
