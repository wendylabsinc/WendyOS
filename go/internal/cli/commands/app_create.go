package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// `wendy app create` asks the dashboard's questions, in its order and words.
// The lifecycle question waits for its contract.

// The questions and the session; tests replace them.
var (
	askText = func(prompt, hint, def string, validate tui.ValidateFunc) (string, error) {
		return tui.PromptTextWithDefault(prompt, hint, def, validate)
	}
	askConfirm   = func(question string) (bool, error) { return tui.ConfirmDefaultNo(question) }
	pickAppAuth  = pickAuthEntry
	withAppCloud = withCloud
)

const notifyHint = "This app may send notifications. People with access to this app can receive them."

var newAppIDPattern = regexp.MustCompile(`^[a-z0-9.-]+$`)

func validateNewAppID(id string) error {
	if !newAppIDPattern.MatchString(id) || appconfig.ValidateAppID(id) != nil {
		return fmt.Errorf("%q is not a valid app id. Use lowercase letters, digits, dots and hyphens.", id)
	}
	return nil
}

// appRoles are the built-in app roles, by the short name --access takes.
var appRoles = map[string]string{"owner": "App owner", "developer": "App developer", "operator": "App operator", "viewer": "App viewer"}

type appGrant struct{ who, role string } // role: the built-in role's name

// parseGrant reads who=role. Who is a member's email or a realm group spelled
// <realm-issuer>#<group>, as bindings name it.
func parseGrant(s string) (appGrant, error) {
	i := strings.LastIndex(s, "=")
	if i < 0 {
		return appGrant{}, fmt.Errorf("%q: give who=role, for example ana@example.com=developer", s)
	}
	who, role := strings.TrimSpace(s[:i]), strings.ToLower(strings.TrimSpace(s[i+1:]))
	name, ok := appRoles[strings.TrimPrefix(role, "app ")]
	if !ok {
		return appGrant{}, fmt.Errorf("%q: the role is one of owner, developer, operator, viewer", s)
	}
	if !strings.Contains(who, "@") && !strings.Contains(who, "#") {
		return appGrant{}, fmt.Errorf("%q: name a person by email, or a group as <realm-issuer>#<group>", s)
	}
	return appGrant{who, name}, nil
}

func parseGrants(list []string) ([]appGrant, error) {
	var out []appGrant
	for _, s := range list {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		g, err := parseGrant(s)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

type appAnswers struct {
	name, id, details string
	notify            bool
	access            []string // who=role
	grants            []appGrant
}

// ask fills what the flags (given) did not, in a terminal; without one a
// missing required answer names its flag.
func (a *appAnswers) ask(given func(string) bool) error {
	terminal := isInteractiveTerminal()
	section := func(title string, flags ...string) {
		for _, f := range flags {
			if terminal && !given(f) {
				fmt.Fprintln(tui.PromptWriter(), "\n"+title)
				return
			}
		}
	}
	text := func(flag, what, prompt, hint string, v *string, required bool, check tui.ValidateFunc) error {
		switch {
		case given(flag):
			if check != nil {
				return markUsage(check(*v))
			}
			return nil
		case !terminal && required:
			return usageErrorf("No %s given. Add --%s <%s>, or run in a terminal to answer.", what, flag, strings.TrimPrefix(what, "app "))
		case !terminal:
			return nil
		}
		ans, err := askText(prompt, hint, *v, check)
		*v = ans
		return err
	}
	nonEmpty := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("a name is required")
		}
		return nil
	}

	section("What is the app called?", "name", "app")
	if err := text("name", "name", "Name", "Shown in lists and on devices.", &a.name, true, nonEmpty); err != nil {
		return err
	}
	if err := text("app", "app id", "App id", "Use the same id in your project's wendy.json. Lowercase letters, digits, dots and hyphens.", &a.id, true, validateNewAppID); err != nil {
		return err
	}

	section("What does the app do?", "details", "notifications")
	if err := text("details", "details", "What it does", "Optional.", &a.details, false, nil); err != nil {
		return err
	}
	if terminal && !given("notifications") {
		ok, err := askConfirm(notifyHint + " Allow?")
		if err != nil {
			return err
		}
		a.notify = ok
	}

	section("Who has access to this app?", "access")
	if terminal && !given("access") {
		fmt.Fprintln(tui.PromptWriter(), "You are its owner. Add the people and groups who work with it.")
		list, err := askText("People and groups", "who=role, separated by commas. Who: an email, or <realm-issuer>#<group>. Role: owner, developer, operator or viewer. Empty for none.", "",
			func(s string) error { _, err := parseGrants(strings.Split(s, ",")); return err })
		if err != nil {
			return err
		}
		a.access = strings.Split(list, ",")
	}
	var err error
	a.grants, err = parseGrants(a.access)
	return markUsage(err)
}

func newAppCreateCmd() *cobra.Command {
	var a appAnswers
	var notify, noNotify, yes bool
	var cloudGRPC string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an app in Wendy Cloud",
		Long: "Asks what the app is called, what it does and who has access to it, shows the answers\n" +
			"and creates the app once you confirm. Each answer has a flag; with all of them nothing is asked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if notify && noNotify {
				return usageErrorf("Give --notifications or --no-notifications, not both.")
			}
			a.notify = notify
			if cfg, err := appconfig.LoadFromFile("wendy.json"); err == nil && cfg.AppID != "" {
				if !cmd.Flags().Changed("app") {
					a.id = cfg.AppID
				}
				if !cmd.Flags().Changed("name") {
					a.name = cfg.AppID
				}
			}
			given := func(f string) bool {
				if f == "notifications" {
					return cmd.Flags().Changed(f) || cmd.Flags().Changed("no-notifications")
				}
				return cmd.Flags().Changed(f)
			}
			if err := a.ask(given); err != nil {
				return err
			}
			if !yes && !isInteractiveTerminal() {
				return usageErrorf("Nothing is created without confirming. Add --yes, or run in a terminal to answer.")
			}
			auth, err := pickAppAuth(cloudGRPC)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				if err := createApp(ctx, out, conn, auth, &a, yes); err != nil {
					return err
				}
				fmt.Fprintf(out, "Publish its first release:\n  wendy app publish --app %s --tag 1.0.0\nRun this in the project folder. The tag is the version you want to give this release.\n", a.id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&a.name, "name", "", "Name, shown in lists and on devices (required)")
	cmd.Flags().StringVar(&a.id, "app", "", "App id, the appId in your project's wendy.json (required)")
	cmd.Flags().StringVar(&a.details, "details", "", "What the app does")
	cmd.Flags().BoolVar(&notify, "notifications", false, "The app may send notifications")
	cmd.Flags().BoolVar(&noNotify, "no-notifications", false, "The app sends no notifications (default)")
	cmd.Flags().StringArrayVar(&a.access, "access", nil, "Give access: <email or realm-issuer#group>=<owner|developer|operator|viewer> (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Create without asking to confirm")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

func operatorOrg(auth *config.AuthConfig) (string, error) {
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return "", errors.New("this needs an operator session; run 'wendy auth login'")
	}
	return auth.Certificates[0].TenantUUID(), nil
}

// createApp checks the id is free and every grant resolves, shows the
// answers, and after confirmation (or yes) creates the app, its notification
// choice and its bindings.
func createApp(ctx context.Context, out io.Writer, conn grpc.ClientConnInterface, auth *config.AuthConfig, a *appAnswers, yes bool) error {
	org, err := operatorOrg(auth)
	if err != nil {
		return err
	}
	_, err = cloudpb.NewAppServiceClient(conn).GetApp(ctx, &cloudpb.GetAppRequest{Id: a.id, OrganizationId: org})
	switch status.Code(err) {
	case codes.NotFound:
	case codes.OK:
		return usageErrorf("The app id %s is taken. Choose another one.", a.id)
	default:
		return fmt.Errorf("checking app id %s: %w", a.id, err)
	}
	bindings, err := resolveGrants(ctx, conn, org, a.id, a.grants)
	if err != nil {
		return err
	}

	notifications := "This app sends no notifications"
	if a.notify {
		notifications = notifyHint
	}
	access := []string{"You (App owner)"}
	for _, g := range a.grants {
		access = append(access, g.who+" ("+g.role+")")
	}
	fmt.Fprintf(out, "Review and create\n  Name: %s\n  App id: %s\n  What it does: %s\n  Notifications: %s\n  Access: %s\nNothing is created until you confirm.\n",
		a.name, a.id, a.details, notifications, strings.Join(access, ", "))
	if !yes {
		ok, err := askConfirm("Create app?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("nothing was created")
		}
	}

	upsert := &cloudpb.UpsertAppRequest{Id: a.id, OrganizationId: org, Name: &a.name, Details: &a.details}
	if err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.AppService_UpsertApp_FullMethodName, upsert, new(cloudpb.App)); err != nil {
		return fmt.Errorf("creating %s: %w", a.id, err)
	}
	if a.notify {
		err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.AppService_UpdateApp_FullMethodName,
			&cloudpb.UpdateAppRequest{Id: a.id, OrganizationId: org, CanSendNotifications: proto.Bool(true)}, new(cloudpb.App))
		switch {
		case status.Code(err) == codes.PermissionDenied:
			fmt.Fprintln(out, "Your organization role doesn't allow notifications. Ask an admin.")
		case err != nil:
			return fmt.Errorf("%s is created, but its notifications were not turned on: %w", a.id, err)
		}
	}
	for i, b := range bindings {
		if err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.AccessService_CreateBinding_FullMethodName, b, new(cloudpb.Binding)); err != nil {
			return fmt.Errorf("%s is created, but %s was not given access: %w", a.id, a.grants[i].who, err)
		}
	}
	fmt.Fprintf(out, "%s is created. It has no release yet.\n", a.name)
	return nil
}

// resolveGrants turns each grant into an APP-scope binding: the built-in
// role by name, a group as written, a person by their email among the
// organization's members.
func resolveGrants(ctx context.Context, conn grpc.ClientConnInterface, org, app string, grants []appGrant) ([]*cloudpb.CreateBindingRequest, error) {
	if len(grants) == 0 {
		return nil, nil
	}
	roles, err := cloudpb.NewRoleServiceClient(conn).ListRoles(ctx, &cloudpb.ListRolesRequest{OrganizationId: org})
	if err != nil {
		return nil, fmt.Errorf("reading the app roles: %w", err)
	}
	var emails map[string]string // lower-case email -> user id, read once
	var out []*cloudpb.CreateBindingRequest
	for _, g := range grants {
		b := &cloudpb.CreateBindingRequest{OrganizationId: org, ScopeType: cloudpb.BindingScopeType_BINDING_SCOPE_TYPE_APP, ScopeRef: app, Effect: cloudpb.BindingEffect_BINDING_EFFECT_ALLOW}
		for _, r := range roles.GetRoles() {
			if r.GetIsBuiltin() && r.GetKind() == cloudpb.RoleKind_ROLE_KIND_APP && r.GetName() == g.role {
				b.RoleId = r.GetId()
			}
		}
		if b.RoleId == "" {
			return nil, fmt.Errorf("your organization has no %s role", g.role)
		}
		if strings.Contains(g.who, "#") {
			b.PrincipalType, b.PrincipalId = cloudpb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_GROUP, g.who
		} else {
			if emails == nil {
				if emails, err = memberEmails(ctx, conn, org); err != nil {
					return nil, err
				}
			}
			b.PrincipalType, b.PrincipalId = cloudpb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER, emails[strings.ToLower(g.who)]
			if b.PrincipalId == "" {
				return nil, fmt.Errorf("%s is not a member of your organization. Invite them first", g.who)
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// memberEmails names the organization's members as the dashboard does: their
// user ids, then each user's email.
// ponytail: one GetUser per member; a batch lookup when the contract has one.
func memberEmails(ctx context.Context, conn grpc.ClientConnInterface, org string) (map[string]string, error) {
	members, err := cloudpb.NewOrganizationServiceClient(conn).ListMembers(ctx, &cloudpb.ListMembersRequest{OrganizationId: org})
	if err != nil {
		return nil, fmt.Errorf("reading your organization's members: %w", err)
	}
	users := cloudpb.NewUserServiceClient(conn)
	out := map[string]string{}
	for _, m := range members.GetMembers() {
		u, err := users.GetUser(ctx, &cloudpb.GetUserRequest{Id: m.GetUserId()})
		if err != nil {
			return nil, fmt.Errorf("reading your organization's members: %w", err)
		}
		out[strings.ToLower(u.GetEmail())] = m.GetUserId()
	}
	return out, nil
}
