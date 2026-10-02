package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/wifiregulatory"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

func newWifiRegulatoryCmd() *cobra.Command {
	root := &cobra.Command{Use: "regulatory", Short: "Show global and effective PHY Wi-Fi regulatory state", Args: cobra.NoArgs}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return runWiFiRegulatory(cmd, "") }
	request := &cobra.Command{Use: "set-country COUNTRY", Short: "Submit an explicit runtime country hint (ISO 3166-1 alpha-2)", Args: cobra.ExactArgs(1)}
	request.RunE = func(cmd *cobra.Command, args []string) error {
		country, err := wifiregulatory.Country(args[0])
		if err != nil {
			return err
		}
		return runWiFiRegulatory(cmd, country)
	}
	root.AddCommand(request)
	return root
}

func runWiFiRegulatory(cmd *cobra.Command, country string) error {
	target, err := resolveTarget(cmd.Context(), ExcludeProviders("local", "docker"), SuppressUpdateCheck())
	if err != nil {
		return err
	}
	defer target.Close()
	if target.Agent == nil || target.Agent.Conn == nil {
		return fmt.Errorf("Wi-Fi regulatory controls require a WendyOS agent connection")
	}
	client := pb.NewWendyWiFiServiceClient(target.Agent.Conn)
	var response *pb.WiFiRegulatoryStatus
	if country == "" {
		response, err = client.GetWiFiRegulatory(cmd.Context(), &pb.GetWiFiRegulatoryRequest{})
	} else {
		response, err = client.RequestWiFiCountry(cmd.Context(), &pb.RequestWiFiCountryRequest{CountryCode: country})
	}
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return fmt.Errorf("this agent does not support Wi-Fi regulatory controls; update to a compatible Linux agent")
		}
		return err
	}
	return printWiFiRegulatory(cmd, response)
}

func printWiFiRegulatory(cmd *cobra.Command, response *pb.WiFiRegulatoryStatus) error {
	if jsonOutput {
		data, err := (protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}).Marshal(response)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if response.RequestedCountry != "" {
		cmd.Printf("Requested country hint: %s\n", response.RequestedCountry)
	}
	cmd.Printf("Global country hint: %s\n", response.GlobalCountry)
	for _, phy := range response.Phys {
		cmd.Printf("%s: country=%s source=%s self-managed=%t\n", phy.Name, phy.Country, phy.RegulatorySource, phy.SelfManaged)
		for _, channel := range phy.Channels {
			cmd.Printf("  %g MHz %s\n", channel.FrequencyMhz, channel.Constraints)
		}
	}
	cmd.Println(response.Detail)
	return nil
}
