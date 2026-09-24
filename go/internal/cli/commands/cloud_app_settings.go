package commands

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

type appSettingsFlags struct {
	cloudGRPC    string
	organization int32
	device       int32
}

func newCloudAppSettingsCmd() *cobra.Command {
	flags := &appSettingsFlags{}
	cmd := &cobra.Command{
		Use:   "app-settings",
		Short: "Manage organization and device App Settings",
	}
	cmd.PersistentFlags().StringVar(&flags.cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set)")
	cmd.PersistentFlags().Int32Var(&flags.organization, "org", 0, "Organization scope (defaults to the authenticated organization)")
	cmd.PersistentFlags().Int32Var(&flags.device, "device", 0, "Device scope by Cloud asset ID")
	cmd.AddCommand(
		newCloudAppSettingsListCmd(flags),
		newCloudAppSettingsGetCmd(flags),
		newCloudAppSettingsSetCmd(flags),
		newCloudAppSettingsClearCmd(flags),
		newCloudAppSettingsActionCmd(flags),
	)
	return cmd
}

func newCloudAppSettingsListCmd(flags *appSettingsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List apps that declare settings for a scope",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withAppSettingsClient(cmd.Context(), flags, func(ctx context.Context, client cloudpb.AppSettingsServiceClient, scope *cloudpb.AppSettingsScope) error {
				request := &cloudpb.AppSettingsListRequest{Scope: scope, Limit: 100}
				apps := []*cloudpb.App{}
				anchors := map[string]bool{}
				for {
					response, err := client.AppSettingsList(ctx, request)
					if err != nil {
						return fmt.Errorf("listing App Settings: %w", err)
					}
					apps = append(apps, response.Apps...)
					if !response.MoreAvailable {
						break
					}
					if len(response.Apps) == 0 {
						return fmt.Errorf("Cloud reported another App Settings page without an anchor")
					}
					anchor := response.Apps[len(response.Apps)-1].Id
					if anchor == "" || anchors[anchor] {
						return fmt.Errorf("Cloud returned an invalid App Settings page anchor")
					}
					anchors[anchor] = true
					request.BeforeAppId = proto.String(anchor)
				}
				if jsonOutput {
					return printAppSettingsJSON(&cloudpb.AppSettingsListResponse{Apps: apps})
				}
				if len(apps) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No apps declare settings for this scope.")
					return nil
				}
				for _, app := range apps {
					name := app.Name
					if name == "" {
						name = app.Id
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", app.Id, name)
				}
				return nil
			})
		},
	}
}

func newCloudAppSettingsGetCmd(flags *appSettingsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <app-id>",
		Short: "Show an app's settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAppSettingsClient(cmd.Context(), flags, func(ctx context.Context, client cloudpb.AppSettingsServiceClient, scope *cloudpb.AppSettingsScope) error {
				document, err := client.AppSettingsGet(ctx, &cloudpb.AppSettingsGetRequest{AppId: args[0], Scope: scope})
				if err != nil {
					return fmt.Errorf("getting App Settings: %w", err)
				}
				if jsonOutput {
					return printAppSettingsJSON(document)
				}
				printAppSettingsDocument(cmd, document)
				return nil
			})
		},
	}
}

func newCloudAppSettingsSetCmd(flags *appSettingsFlags) *cobra.Command {
	var restart bool
	cmd := &cobra.Command{
		Use:   "set <app-id> <key=value>...",
		Short: "Set one or more typed settings atomically",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAppSettingsClient(cmd.Context(), flags, func(ctx context.Context, client cloudpb.AppSettingsServiceClient, scope *cloudpb.AppSettingsScope) error {
				document, err := client.AppSettingsGet(ctx, &cloudpb.AppSettingsGetRequest{AppId: args[0], Scope: scope})
				if err != nil {
					return fmt.Errorf("getting App Settings schema: %w", err)
				}
				changes, err := appSettingsChanges(document, args[1:])
				if err != nil {
					return err
				}
				_, err = client.AppSettingsUpdate(ctx, &cloudpb.AppSettingsUpdateRequest{
					AppId: args[0], Scope: scope, Changes: changes, RestartIfRequired: restart,
				})
				if err != nil {
					return fmt.Errorf("updating App Settings: %w", err)
				}
				if jsonOutput {
					return printAppSettingsJSON(&cloudpb.AppSettingsUpdateResponse{})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "App Settings updated.")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&restart, "restart", false, "Restart the app when a changed setting requires it")
	return cmd
}

func newCloudAppSettingsClearCmd(flags *appSettingsFlags) *cobra.Command {
	var restart bool
	cmd := &cobra.Command{
		Use:   "clear <app-id> <key>...",
		Short: "Clear scoped values so inherited or default values apply",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAppSettingsClient(cmd.Context(), flags, func(ctx context.Context, client cloudpb.AppSettingsServiceClient, scope *cloudpb.AppSettingsScope) error {
				changes := make(map[string]*cloudpb.AppSettingsValue, len(args)-1)
				for _, key := range args[1:] {
					if strings.TrimSpace(key) == "" {
						return fmt.Errorf("setting keys cannot be empty")
					}
					changes[key] = &cloudpb.AppSettingsValue{Value: &cloudpb.AppSettingsValue_Null{Null: structpb.NullValue_NULL_VALUE}}
				}
				_, err := client.AppSettingsUpdate(ctx, &cloudpb.AppSettingsUpdateRequest{
					AppId: args[0], Scope: scope, Changes: changes, RestartIfRequired: restart,
				})
				if err != nil {
					return fmt.Errorf("clearing App Settings: %w", err)
				}
				if jsonOutput {
					return printAppSettingsJSON(&cloudpb.AppSettingsUpdateResponse{})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "App Settings cleared.")
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&restart, "restart", false, "Restart the app when a cleared setting requires it")
	return cmd
}

func newCloudAppSettingsActionCmd(flags *appSettingsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "action <app-id> <control-key>",
		Short: "Perform an App Settings button action",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withAppSettingsClient(cmd.Context(), flags, func(ctx context.Context, client cloudpb.AppSettingsServiceClient, scope *cloudpb.AppSettingsScope) error {
				_, err := client.AppSettingsPerformControlAction(ctx, &cloudpb.AppSettingsPerformControlActionRequest{
					AppId: args[0], Scope: scope, ControlKey: args[1],
				})
				if err != nil {
					return fmt.Errorf("performing App Settings action: %w", err)
				}
				if jsonOutput {
					return printAppSettingsJSON(&cloudpb.AppSettingsPerformControlActionResponse{})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "App Settings action performed.")
				return nil
			})
		},
	}
}

func withAppSettingsClient(
	ctx context.Context,
	flags *appSettingsFlags,
	operation func(context.Context, cloudpb.AppSettingsServiceClient, *cloudpb.AppSettingsScope) error,
) error {
	auth, err := pickAuthEntry(flags.cloudGRPC)
	if err != nil {
		return err
	}
	authenticatedOrganization := 0
	if len(auth.Certificates) > 0 {
		authenticatedOrganization = auth.Certificates[0].OrganizationID
	}
	scope, err := appSettingsScope(flags, authenticatedOrganization)
	if err != nil {
		return err
	}
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	return operation(cloudCtx, cloudpb.NewAppSettingsServiceClient(conn), scope)
}

func appSettingsScope(flags *appSettingsFlags, authenticatedOrganization int) (*cloudpb.AppSettingsScope, error) {
	if flags.organization > 0 && flags.device > 0 {
		return nil, fmt.Errorf("--org and --device are mutually exclusive")
	}
	if flags.device > 0 {
		return &cloudpb.AppSettingsScope{Value: &cloudpb.AppSettingsScope_Device{Device: &cloudpb.AppSettingsScopeDevice{Id: flags.device}}}, nil
	}
	organization := flags.organization
	if organization == 0 {
		organization = int32(authenticatedOrganization)
	}
	if organization <= 0 {
		return nil, fmt.Errorf("an organization or device scope is required")
	}
	return &cloudpb.AppSettingsScope{Value: &cloudpb.AppSettingsScope_Organization{Organization: &cloudpb.AppSettingsScopeOrganization{Id: organization}}}, nil
}

func appSettingsChanges(document *cloudpb.AppSettings, assignments []string) (map[string]*cloudpb.AppSettingsValue, error) {
	controls := make(map[string]*cloudpb.AppSettingsControl, len(document.Controls))
	for _, control := range document.Controls {
		controls[control.Key] = control
	}
	changes := make(map[string]*cloudpb.AppSettingsValue, len(assignments))
	for _, assignment := range assignments {
		key, raw, ok := strings.Cut(assignment, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("expected key=value, got %q", assignment)
		}
		control := controls[key]
		if control == nil {
			return nil, fmt.Errorf("unknown setting %q", key)
		}
		value, err := parseAppSettingsValue(control, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		changes[key] = value
	}
	return changes, nil
}

func parseAppSettingsValue(control *cloudpb.AppSettingsControl, raw string) (*cloudpb.AppSettingsValue, error) {
	switch typed := control.Control.(type) {
	case *cloudpb.AppSettingsControl_Toggle:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected true or false")
		}
		return &cloudpb.AppSettingsValue{Value: &cloudpb.AppSettingsValue_Toggle{Toggle: value}}, nil
	case *cloudpb.AppSettingsControl_SingleSelect:
		for _, option := range typed.SingleSelect.Options {
			if option.Key == raw {
				return &cloudpb.AppSettingsValue{Value: &cloudpb.AppSettingsValue_SingleSelect{SingleSelect: raw}}, nil
			}
		}
		return nil, fmt.Errorf("expected one of %s", optionKeys(typed.SingleSelect.Options))
	case *cloudpb.AppSettingsControl_MultiSelect:
		keys := []string{}
		if raw != "" {
			keys = strings.Split(raw, ",")
		}
		allowed := make(map[string]bool, len(typed.MultiSelect.Options))
		for _, option := range typed.MultiSelect.Options {
			allowed[option.Key] = true
		}
		seen := map[string]bool{}
		for _, key := range keys {
			if !allowed[key] || seen[key] {
				return nil, fmt.Errorf("expected unique comma-separated values from %s", optionKeys(typed.MultiSelect.Options))
			}
			seen[key] = true
		}
		return &cloudpb.AppSettingsValue{Value: &cloudpb.AppSettingsValue_MultiSelect{MultiSelect: &cloudpb.AppSettingsValueMultiSelect{Keys: keys}}}, nil
	case *cloudpb.AppSettingsControl_Slider:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < typed.Slider.Minimum || value > typed.Slider.Maximum {
			return nil, fmt.Errorf("expected a number between %g and %g", typed.Slider.Minimum, typed.Slider.Maximum)
		}
		return &cloudpb.AppSettingsValue{Value: &cloudpb.AppSettingsValue_Slider{Slider: value}}, nil
	default:
		return nil, fmt.Errorf("control is read-only or an action")
	}
}

func optionKeys(options []*cloudpb.AppSettingsControlSelectOption) string {
	keys := make([]string, 0, len(options))
	for _, option := range options {
		keys = append(keys, option.Key)
	}
	return strings.Join(keys, ", ")
}

func printAppSettingsDocument(cmd *cobra.Command, document *cloudpb.AppSettings) {
	sections := map[string]string{"": "Settings"}
	order := []string{""}
	for _, section := range document.Sections {
		title := section.GetTitle()
		if title == "" {
			title = section.Key
		}
		sections[section.Key] = title
		order = append(order, section.Key)
	}
	grouped := map[string][]*cloudpb.AppSettingsControl{}
	for _, control := range document.Controls {
		grouped[control.GetSection()] = append(grouped[control.GetSection()], control)
	}
	for _, section := range order {
		if len(grouped[section]) == 0 {
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", sections[section])
		for _, control := range grouped[section] {
			fmt.Fprintf(cmd.OutOrStdout(), "  %-24s %s\n", control.Key, appSettingsDisplayValue(control))
		}
	}
}

func appSettingsDisplayValue(control *cloudpb.AppSettingsControl) string {
	switch typed := control.Control.(type) {
	case *cloudpb.AppSettingsControl_Toggle:
		if typed.Toggle.Value == nil {
			return "unavailable"
		}
		return strconv.FormatBool(typed.Toggle.GetValue())
	case *cloudpb.AppSettingsControl_SingleSelect:
		if typed.SingleSelect.Value == nil {
			return "unavailable"
		}
		return typed.SingleSelect.GetValue().GetTitle()
	case *cloudpb.AppSettingsControl_MultiSelect:
		titles := make([]string, 0, len(typed.MultiSelect.Value))
		for _, option := range typed.MultiSelect.Value {
			titles = append(titles, option.Title)
		}
		return strings.Join(titles, ", ")
	case *cloudpb.AppSettingsControl_Slider:
		if typed.Slider.Value == nil {
			return "unavailable"
		}
		return strconv.FormatFloat(typed.Slider.GetValue(), 'f', -1, 64)
	case *cloudpb.AppSettingsControl_Button:
		return "action"
	case *cloudpb.AppSettingsControl_Status:
		return strings.TrimPrefix(strings.ToLower(typed.Status.State.String()), "app_settings_control_status_state_")
	default:
		return "unavailable"
	}
}

func printAppSettingsJSON(message proto.Message) error {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(message)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
