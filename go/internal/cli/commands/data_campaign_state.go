package commands

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDataCampaignArmCmd() *cobra.Command {
	return &cobra.Command{Use: "arm <campaign>", Short: "Switch a disarmed campaign back on",
		Long: "Arm a disarmed campaign again: its triggers fire, its inference restarts and its camera pre-roll resumes.\nArming a campaign that is already armed changes nothing.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				response, err := client.CampaignArm(cmd.Context(), &agentpbv2.DataCampaignArmRequest{Name: args[0]})
				if err != nil {
					return campaignStateCallError("arm", err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), response)
				}
				return writeCampaignArmed(cmd.OutOrStdout(), response)
			})
		}}
}

func newDataCampaignDisarmCmd() *cobra.Command {
	return &cobra.Command{Use: "disarm <campaign>", Short: "Switch a campaign off without removing it",
		Long: "Disarm a deployed campaign: its triggers stop firing, its inference stops and its camera pre-roll is released,\n" +
			"so the camera is no longer held for it. The plan, its revision and every recorded episode are kept, and the\n" +
			"campaign stays disarmed across agent restarts, reboots and redeploys until it is armed again.\n" +
			"A recording in progress is not cut short: it finishes its after_trigger window and is sealed normally.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				response, err := client.CampaignDisarm(cmd.Context(), &agentpbv2.DataCampaignDisarmRequest{Name: args[0]})
				if err != nil {
					return campaignStateCallError("disarm", err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), response)
				}
				return writeCampaignDisarmed(cmd.OutOrStdout(), response)
			})
		}}
}

func newDataCampaignRemoveCmd() *cobra.Command {
	var force bool
	command := &cobra.Command{Use: "remove <campaign>", Aliases: []string{"rm"}, Short: "Stop a campaign and delete its plan, keeping its episodes",
		Long: "Remove a deployed campaign: stop it, delete its stored plan and drop it from the list.\n" +
			"Every episode it recorded is kept, and still uploads under the plan's upload policy.\n" +
			"A campaign that is recording is refused unless --force is given, which stops and seals the recording first.\n" +
			"A campaign deployed for a fleet comes back if the plan is deployed again; disarm it instead to keep it off.",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				response, err := client.CampaignRemove(cmd.Context(), &agentpbv2.DataCampaignRemoveRequest{Name: args[0], Force: force})
				if err != nil {
					return campaignStateCallError("remove", err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), response)
				}
				return writeCampaignRemoved(cmd.OutOrStdout(), response)
			})
		}}
	command.Flags().BoolVar(&force, "force", false, "Stop and seal the campaign's recording in progress, then remove it")
	return command
}

// campaignStateCallError explains an agent that predates arm, disarm and
// remove. Other errors are the agent's own, which already name the campaign.
func campaignStateCallError(verb string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this device's agent does not support `wendy data campaign %s`; update it with `wendy device update`", verb)
	}
	return err
}

func writeCampaignArmed(out io.Writer, response *agentpbv2.DataCampaignArmResponse) error {
	campaign := response.GetCampaign()
	if !response.GetChanged() {
		_, err := fmt.Fprintf(out, "Campaign %s is already armed (revision %s).\n", campaign.GetName(), shortRevision(campaign.GetRevision()))
		return err
	}
	_, err := fmt.Fprintf(out, "Campaign %s: %s (revision %s)\nIts triggers, inference and camera pre-roll are running again.\n",
		campaign.GetName(), campaign.GetState(), shortRevision(campaign.GetRevision()))
	return err
}

func writeCampaignDisarmed(out io.Writer, response *agentpbv2.DataCampaignDisarmResponse) error {
	campaign := response.GetCampaign()
	if !response.GetChanged() {
		fmt.Fprintf(out, "Campaign %s is already disarmed (revision %s).\n", campaign.GetName(), shortRevision(campaign.GetRevision()))
	} else {
		fmt.Fprintf(out, "Campaign %s: %s (revision %s)\n", campaign.GetName(), campaign.GetState(), shortRevision(campaign.GetRevision()))
		fmt.Fprintln(out, "Its triggers and inference are stopped and its camera pre-roll is released. The plan and its episodes are kept.")
	}
	for _, warning := range response.GetWarnings() {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	_, err := fmt.Fprintf(out, "Arm it again with: wendy data campaign arm %s\n", campaign.GetName())
	return err
}

func writeCampaignRemoved(out io.Writer, response *agentpbv2.DataCampaignRemoveResponse) error {
	if sealed := response.GetSealed(); sealed != nil {
		fmt.Fprintf(out, "Stopped and sealed its recording first: episode %s (%s).\n", sealed.GetId(), sealed.GetState())
	}
	fmt.Fprintf(out, "Removed campaign %s (revision %s).\n", response.GetName(), shortRevision(response.GetRevision()))
	fmt.Fprintln(out, "Its episodes are kept; list them with: wendy data episodes")
	for _, warning := range response.GetWarnings() {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
	return nil
}
