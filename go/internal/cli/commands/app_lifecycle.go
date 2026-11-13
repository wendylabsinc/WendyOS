package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Lifecycle policy (spec 6.9, 6.10): how many stored releases of an app are
// kept. An app's override sets some fields; an unset field inherits the
// organization default, then the platform default. Cloud gives the limits.

var policyFields = []struct {
	flag, label, help string
	field             func(*cloudpb.LifecyclePolicy) **int32
}{
	{"keep-tagged", "Tagged releases to keep", "Lifecycle: keep this many most recent tagged releases",
		func(p *cloudpb.LifecyclePolicy) **int32 { return &p.KeepTagged }},
	{"keep-untagged", "Untagged releases to keep", "Lifecycle: keep this many most recent untagged releases",
		func(p *cloudpb.LifecyclePolicy) **int32 { return &p.KeepUntagged }},
	{"untagged-max-age-days", "Delete untagged releases older than (days)", "Lifecycle: delete untagged releases older than this many days",
		func(p *cloudpb.LifecyclePolicy) **int32 { return &p.UntaggedMaxAgeDays }},
}

// value reads a field of a policy that may be nil.
func value(p *cloudpb.LifecyclePolicy, field func(*cloudpb.LifecyclePolicy) **int32) *int32 {
	if p == nil {
		return nil
	}
	return *field(p)
}

func bindPolicyFlags(f *pflag.FlagSet) {
	for _, pf := range policyFields {
		f.Int32(pf.flag, 0, pf.help)
	}
	f.Bool("org-default", false, "Lifecycle: use the organization default")
}

// policyFromFlags is base with the flags given applied, nil when none is
// given, and empty (every field inherits) for --org-default.
func policyFromFlags(f *pflag.FlagSet, base *cloudpb.LifecyclePolicy) (*cloudpb.LifecyclePolicy, error) {
	var p *cloudpb.LifecyclePolicy
	for _, pf := range policyFields {
		if !f.Changed(pf.flag) {
			continue
		}
		if p == nil {
			p = &cloudpb.LifecyclePolicy{}
			if base != nil {
				p = proto.Clone(base).(*cloudpb.LifecyclePolicy)
			}
		}
		v, _ := f.GetInt32(pf.flag)
		*pf.field(p) = &v
	}
	if f.Changed("org-default") {
		if p != nil {
			return nil, usageErrorf("Give --org-default or a lifecycle value, not both.")
		}
		return &cloudpb.LifecyclePolicy{}, nil
	}
	return p, nil
}

// checkPolicy refuses a set value below 1 or above its limit.
func checkPolicy(p, limit *cloudpb.LifecyclePolicy) error {
	for _, pf := range policyFields {
		v := *pf.field(p)
		if v == nil {
			continue
		}
		if err := checkPolicyValue(*v, value(limit, pf.field)); err != nil {
			return usageErrorf("--%s: %v", pf.flag, err)
		}
	}
	return nil
}

func checkPolicyValue(v int32, limit *int32) error {
	if v < 1 {
		return errors.New("enter 1 or more")
	}
	if limit != nil && v > *limit {
		return fmt.Errorf("enter %d or less. Your organization allows up to %d", *limit, *limit)
	}
	return nil
}

// createLimit is the most a new app's override may set: per field, the
// organization's per-app limit under the plan limit.
func createLimit(s *cloudpb.LifecycleSettings) *cloudpb.LifecyclePolicy {
	limit := &cloudpb.LifecyclePolicy{}
	if s.GetPlanLimit() != nil {
		limit = proto.Clone(s.GetPlanLimit()).(*cloudpb.LifecyclePolicy)
	}
	for _, pf := range policyFields {
		if org := value(s.GetPerAppLimit(), pf.field); org != nil && (*pf.field(limit) == nil || *org < **pf.field(limit)) {
			*pf.field(limit) = org
		}
	}
	return limit
}

// policyLines describes a policy, with its limit when known.
func policyLines(p, limit *cloudpb.LifecyclePolicy) []string {
	if p == nil {
		p = &cloudpb.LifecyclePolicy{}
	}
	n := func(v *int32, format string) string {
		if v == nil {
			return "the platform default"
		}
		return fmt.Sprintf(format, *v)
	}
	tagged := "Tagged releases: " + n(p.KeepTagged, "keep the last %d") + "."
	untagged := "Untagged releases: " + n(p.KeepUntagged, "keep the last %d") + ", and " + n(p.UntaggedMaxAgeDays, "none older than %d days") + "."
	if limit != nil {
		tagged += " It may keep up to " + n(limit.KeepTagged, "%d") + "."
		untagged += " It may keep up to " + n(limit.KeepUntagged, "%d") + ", for up to " + n(limit.UntaggedMaxAgeDays, "%d days") + "."
	}
	return []string{tagged, untagged}
}

// policySummary is the review line: the organization default, or the
// app's own values.
func policySummary(p *cloudpb.LifecyclePolicy) string {
	if p == nil {
		p = &cloudpb.LifecyclePolicy{}
	}
	var parts []string
	if p.KeepTagged != nil {
		parts = append(parts, fmt.Sprintf("keep %d tagged", *p.KeepTagged))
	}
	if p.KeepUntagged != nil {
		parts = append(parts, fmt.Sprintf("keep %d untagged", *p.KeepUntagged))
	}
	if p.UntaggedMaxAgeDays != nil {
		parts = append(parts, fmt.Sprintf("untagged none older than %d days", *p.UntaggedMaxAgeDays))
	}
	if len(parts) == 0 {
		return "Organization default"
	}
	return "Own policy · " + strings.Join(parts, " · ")
}

// askPolicy is create step 4 in a terminal: the organization default, or a
// policy for this app within the limit.
func askPolicy(s *cloudpb.LifecycleSettings) (*cloudpb.LifecyclePolicy, error) {
	w := tui.PromptWriter()
	fmt.Fprintln(w, "\nHow long are its releases kept?")
	fmt.Fprintln(w, "Old releases are deleted by this policy. A release that a deployment uses is always kept.")
	fmt.Fprintln(w, "The organization default:")
	for _, l := range policyLines(s.GetDefaultPolicy(), nil) {
		fmt.Fprintln(w, "  "+l)
	}
	choice, err := askText("Use the organization default, or set a policy for this app?", "default or own.", "default", func(s string) error {
		if s != "default" && s != "own" {
			return errors.New("answer default or own")
		}
		return nil
	})
	if err != nil || choice == "default" {
		return &cloudpb.LifecyclePolicy{}, err
	}
	limit, p := createLimit(s), &cloudpb.LifecyclePolicy{}
	for _, pf := range policyFields {
		def, hint := "", ""
		if v := value(s.GetDefaultPolicy(), pf.field); v != nil {
			def = strconv.Itoa(int(*v))
		}
		if l := *pf.field(limit); l != nil {
			hint = fmt.Sprintf("Up to %d.", *l)
		}
		ans, err := askText(pf.label, hint, def, func(s string) error {
			v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
			if err != nil {
				return errors.New("enter a whole number")
			}
			return checkPolicyValue(int32(v), *pf.field(limit))
		})
		if err != nil {
			return nil, err
		}
		v, _ := strconv.ParseInt(strings.TrimSpace(ans), 10, 32)
		*pf.field(p) = proto.Int32(int32(v))
	}
	return p, nil
}

func lifecycleSettings(ctx context.Context, conn grpc.ClientConnInterface, org string) (*cloudpb.LifecycleSettings, error) {
	s, err := cloudpb.NewOrganizationServiceClient(conn).GetLifecycleSettings(ctx, &cloudpb.GetLifecycleSettingsRequest{OrganizationId: org})
	if err != nil {
		return nil, fmt.Errorf("reading your organization's lifecycle settings: %w", err)
	}
	return s, nil
}

func newAppLifecycleCmd() *cobra.Command {
	var yes bool
	var cloudGRPC string
	cmd := &cobra.Command{
		Use:   "lifecycle <app>",
		Short: "Show or set how long an app's releases are kept",
		Long: "Without flags, shows the app's lifecycle policy, where it comes from and its limits.\n" +
			"With --keep-tagged, --keep-untagged or --untagged-max-age-days, sets those values for this app;\n" +
			"--org-default makes the app use the organization default again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			auth, err := pickAppAuth(cloudGRPC)
			if err != nil {
				return err
			}
			org, err := operatorOrg(auth)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				app, err := cloudpb.NewAppServiceClient(conn).GetApp(ctx, &cloudpb.GetAppRequest{Id: args[0], OrganizationId: org})
				if status.Code(err) == codes.NotFound {
					return usageErrorf("No app %s in this organization. Create it with wendy app create.", args[0])
				}
				if err != nil {
					return fmt.Errorf("reading %s: %w", args[0], err)
				}
				p, err := policyFromFlags(cmd.Flags(), app.GetLifecyclePolicy())
				if err != nil || p == nil {
					if err == nil {
						showLifecycle(out, app)
					}
					return err
				}
				if err := checkPolicy(p, app.GetLifecyclePolicyLimit()); err != nil {
					return err
				}
				fmt.Fprintf(out, "Lifecycle policy of %s: %s → %s\n", app.GetId(), policySummary(app.GetLifecyclePolicy()), policySummary(p))
				if !yes {
					if !isInteractiveTerminal() {
						return usageErrorf("Not confirmed. Add --yes to apply it without a terminal.")
					}
					ok, err := askConfirm("Apply this lifecycle policy?")
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("nothing was changed")
					}
				}
				updated := new(cloudpb.App)
				if err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.AppService_UpdateApp_FullMethodName,
					&cloudpb.UpdateAppRequest{Id: app.GetId(), OrganizationId: org, LifecyclePolicy: p}, updated); err != nil {
					return fmt.Errorf("setting the lifecycle policy of %s: %w", app.GetId(), err)
				}
				showLifecycle(out, updated)
				return nil
			})
		},
	}
	bindPolicyFlags(cmd.Flags())
	cmd.Flags().BoolVar(&yes, "yes", false, "Apply without asking for confirmation")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

func showLifecycle(out io.Writer, app *cloudpb.App) {
	source := "Organization default"
	if policySummary(app.GetLifecyclePolicy()) != source {
		source = "Own policy"
	}
	fmt.Fprintf(out, "Lifecycle policy of %s: %s\n", app.GetId(), source)
	for _, l := range policyLines(app.GetEffectiveLifecyclePolicy(), app.GetLifecyclePolicyLimit()) {
		fmt.Fprintln(out, "  "+l)
	}
	fmt.Fprintln(out, "A release that a deployment uses is always kept.")
}
