package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func cloudOrganizationName(ctx context.Context, auth *config.AuthConfig) string {
	cached := cachedCloudOrganizationName(auth)
	if auth == nil {
		return cached
	}
	// A background display lookup must not rotate refresh tokens or mutate
	// the session being used by the picker/device scan. Use the current token;
	// an expired token simply leaves the last known name in place.
	lookupAuth := *auth
	lookupAuth.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
	lookupAuth.OAuthIssuer = ""
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	name := fetchCloudOrganizationName(ctx, &lookupAuth)
	if name == "" {
		return cached
	}
	cacheCloudOrganizationName(auth.CloudGRPC, auth.OrganizationKey(), name)
	return name
}

func fetchCloudOrganizationName(ctx context.Context, auth *config.AuthConfig) string {
	if auth == nil || len(auth.Certificates) == 0 {
		return ""
	}
	if tenant := auth.Certificates[0].TenantUUID(); tenant != "" {
		conn, err := dialCloudGRPC(auth)
		if err != nil {
			return ""
		}
		defer conn.Close()
		cloudCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return ""
		}
		org, err := pb.NewOrganizationServiceClient(conn).GetOrganization(cloudCtx, &pb.GetOrganizationRequest{Id: tenant})
		if err != nil || org.GetId() != tenant {
			return ""
		}
		return org.GetName()
	}
	orgs, err := listOrgsFromCloud(ctx, auth)
	if err != nil {
		return ""
	}
	for _, o := range orgs {
		if o.GetId() == cloudAuthOrgID(auth) {
			return o.GetName()
		}
	}
	return ""
}
func listCloudOrganizationsV2(ctx context.Context, auth *config.AuthConfig) ([]*pb.Organization, error) {
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	client := pb.NewOrganizationServiceClient(conn)
	var orgs []*pb.Organization
	for page := 0; page < orgPageCap; page++ {
		cloudCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return nil, err
		}
		stream, err := client.ListOrganizations(cloudCtx, &pb.ListOrganizationsRequest{Offset: int32Ptr(int32(page * orgPageSize)), Limit: int32Ptr(orgPageSize)})
		if err != nil {
			return nil, err
		}
		received := 0
		for {
			r, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing organizations: %w", err)
			}
			if r.Organization != nil {
				orgs = append(orgs, r.Organization)
				cacheCloudOrganizationName(auth.CloudGRPC, r.Organization.Id, r.Organization.Name)
				received++
			}
		}
		if received < orgPageSize {
			return orgs, nil
		}
	}
	return nil, fmt.Errorf("Cloud returned too many organization pages")
}

var pickCloudOrgV2 = func(orgs []*pb.Organization, auth *config.AuthConfig, cfg *config.Config) (string, error) {
	picker := tui.NewPickerWithTitleAndColumns("Select an organisation", authPickerColumns)
	if cfg.DefaultCloudGRPC == auth.CloudGRPC {
		picker.DefaultKey = cfg.DefaultTenantUUID
	}
	picker.OnSetDefault = func(item tui.PickerItem) string {
		if err := persistSessionDefault(auth.CloudGRPC + "::" + item.Value.(string)); err != nil {
			return fmt.Sprint(err)
		}
		return "Default set to " + item.Name + "."
	}
	var items []tui.PickerItem
	for _, org := range orgs {
		items = append(items, tui.PickerItem{Name: org.Name, Description: org.Id, Type: auth.CloudGRPC, DedupKey: org.Id, Value: org.Id})
	}
	model, _ := picker.Update(tui.PickerAddMsg{Items: items})
	picker = model.(tui.PickerModel)
	model, _ = picker.Update(tui.PickerDoneMsg{})
	picker = model.(tui.PickerModel)
	final, err := tea.NewProgram(picker).Run()
	if err != nil {
		return "", err
	}
	result := final.(tui.PickerModel)
	if result.Cancelled() || result.Selected() == nil {
		return "", ErrUserCancelled
	}
	return result.Selected().Value.(string), nil
}

func switchCloudOrganizationV2(ctx context.Context, cfg *config.Config, source *config.AuthConfig) (*config.AuthConfig, *config.Config, error) {
	orgs, err := listCloudOrganizationsV2(ctx, source)
	if err != nil {
		return nil, cfg, err
	}
	if len(orgs) == 0 {
		return nil, cfg, fmt.Errorf("your account belongs to no organizations")
	}
	id, err := pickCloudOrgV2(orgs, source, cfg)
	if err != nil {
		return nil, cfg, err
	}
	var chosen *pb.Organization
	for _, org := range orgs {
		if org.Id == id {
			chosen = org
			break
		}
	}
	if chosen == nil {
		return nil, cfg, fmt.Errorf("selected organization is no longer available")
	}
	find := func(cfg *config.Config) *config.AuthConfig {
		for i := range cfg.Auth {
			a := &cfg.Auth[i]
			if a.CloudGRPC == source.CloudGRPC && len(a.Certificates) > 0 && a.Certificates[0].TenantUUID() == id {
				return a
			}
		}
		return nil
	}
	if selected := find(cfg); selected != nil {
		if fresh, e := loadCloudOrgConfig(); e == nil {
			if a := find(fresh); a != nil {
				return a, fresh, nil
			}
		}
		return selected, cfg, nil
	}
	fmt.Println(tui.InfoMessage(fmt.Sprintf("No credentials are stored for %s (%s). Complete login and select that organization in the browser.", chosen.Name, id)))
	dashboard, endpoint := loginTargetsForAuth(source)
	if err := performLoginFn(ctx, dashboard, endpoint); err != nil {
		return nil, cfg, err
	}
	fresh, err := loadCloudOrgConfig()
	if err != nil {
		return nil, cfg, err
	}
	if a := find(fresh); a != nil {
		return a, fresh, nil
	}
	return nil, fresh, fmt.Errorf("login completed without credentials for selected organization %s", id)
}
