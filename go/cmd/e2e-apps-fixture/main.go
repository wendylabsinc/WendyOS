// e2e-apps-fixture is test infrastructure, never linked into wendy or the Agent.
// It uses the same generated clients, authentication and signing as the CLI.
// Results are live public observations; it never accepts responses or scripts.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type options struct {
	operation, cloud, tenant, app, name, deployment, release, asset, note string
	account, devices                                                      string
	expected                                                              int
}

type fixtureError struct {
	stage string
	cause error
}

func (e *fixtureError) Error() string              { return "E2E fixture stage failed: " + e.stage }
func (e *fixtureError) Unwrap() error              { return e.cause }
func atFixtureStage(stage string, err error) error { return &fixtureError{stage: stage, cause: err} }

func main() {
	var o options
	flag.StringVar(&o.operation, "operation", "catalog", "Public operation")
	flag.StringVar(&o.cloud, "cloud", "", "Explicit Dev endpoint")
	flag.StringVar(&o.tenant, "tenant", "", "Expected tenant")
	flag.StringVar(&o.app, "app", "", "Owned test app")
	flag.StringVar(&o.name, "name", "workshop", "Deployment name")
	flag.StringVar(&o.deployment, "deployment", "", "Deployment UUID")
	flag.StringVar(&o.release, "release", "", "Release UUID")
	flag.StringVar(&o.asset, "asset", "", "Asset UUID")
	flag.StringVar(&o.note, "note", "E2E pending removal", "Public history note")
	flag.IntVar(&o.expected, "expected-revision", 0, "Expected revision")
	flag.StringVar(&o.account, "account", "", "Existing saved auth context")
	flag.StringVar(&o.devices, "devices", "", "Approved device resource file")
	flag.Parse()
	value, err := run(o)
	if err != nil {
		// Deliberately omit auth structs, raw envelopes and arbitrary server text.
		code := status.Code(err).String()
		diagnostic := map[string]any{"code": code, "operation": o.operation}
		var fixture *fixtureError
		if errors.As(err, &fixture) {
			diagnostic["stage"] = fixture.stage
		}
		_ = json.NewEncoder(os.Stdout).Encode(diagnostic)
		fmt.Fprintln(os.Stderr, "E2E public operation failed; inspect its sanitized code and prerequisites")
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(1)
	}
}

func heldToken(_ context.Context, a *config.AuthConfig) error {
	if a.OAuthIssuer != "" {
		expiry, err := time.Parse(time.RFC3339, a.OAuthExpiresAt)
		if err != nil || time.Until(expiry) < 90*time.Second || !a.HasAPIKey() {
			return atFixtureStage("saved_session_freshness", errors.New("fresh selected session required; adapter never refreshes"))
		}
	}
	return nil
}
func run(o options) (any, error) {
	if o.operation == "pin-approved-devices" {
		return pinApprovedDevices(o)
	}
	if os.Getenv("WENDY_E2E_APPS_DEPLOYMENTS") != "true" || !strings.HasPrefix(o.app, "sh.wendy.e2e.apps-") {
		return nil, errors.New("owned app and explicit opt-in required")
	}
	if !strings.Contains(o.cloud, ".dev.") || !strings.HasSuffix(o.cloud, ":443") || os.Getenv("WENDY_CLOUD_INSECURE") != "" {
		return nil, errors.New("explicit TLS Dev endpoint required")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, errors.New("saved session required")
	}
	var auth *config.AuthConfig
	if account := os.Getenv("WENDY_E2E_APPS_ACCOUNT"); account != "" {
		auth, err = cloudsession.LoadAccount(account, o.tenant)
	} else if len(cfg.Auth) == 1 {
		auth = &cfg.Auth[0]
	}
	if err != nil || auth == nil {
		return nil, errors.New("explicit saved account or dedicated session required")
	}
	if auth.CloudGRPC != o.cloud || len(auth.Certificates) != 1 || auth.Certificates[0].TenantUUID() != o.tenant {
		return nil, errors.New("session identity mismatch")
	}
	if o.operation == "device-profile" {
		return approvedDeviceProfile(auth, o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var out any
	err = cloudsession.With(ctx, auth, heldToken, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		var err error
		switch o.operation {
		case "operator-profile":
			self, e := pb.NewAccessServiceClient(conn).GetEffectivePermissions(ctx, &pb.GetEffectivePermissionsRequest{OrganizationId: o.tenant, PrincipalType: pb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER})
			if e != nil {
				return e
			}
			ids := map[string]bool{}
			for _, permission := range self.Permissions {
				for _, binding := range permission.Bindings {
					if binding.GetPrincipal().GetType() == pb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER && binding.GetPrincipal().GetId() != "" {
						ids[binding.Principal.Id] = true
					}
				}
			}
			if len(ids) != 1 {
				return errors.New("public self-permissions must identify one user")
			}
			var id string
			for value := range ids {
				id = value
			}
			user, e := pb.NewUserServiceClient(conn).GetUser(ctx, &pb.GetUserRequest{Id: id})
			if e != nil {
				return e
			}
			keys, e := pb.NewKeyDirectoryServiceClient(conn).GetRecipientKeys(ctx, &pb.GetRecipientKeysRequest{OrganizationId: o.tenant, Principals: []string{auth.Certificates[0].PrincipalURI}})
			if e != nil {
				return e
			}
			keyCount := 0
			for _, key := range keys.Keys {
				if key.Principal == auth.Certificates[0].PrincipalURI && key.Registration != nil {
					keyCount++
				}
			}
			out = map[string]any{"operatorId": id, "matchesExpectedEmail": user.Email == os.Getenv("WENDY_E2E_APPS_MEMBER_EMAIL"), "registeredKeyCount": keyCount}
		case "roles":
			var response *pb.ListRolesResponse
			response, err = pb.NewRoleServiceClient(conn).ListRoles(ctx, &pb.ListRolesRequest{OrganizationId: o.tenant})
			if err == nil {
				out = wire(response)
			}
		case "catalog":
			out, err = catalog(ctx, conn, auth, o)
		case "deployment":
			out, err = deployment(ctx, conn, o)
		case "asset-apps":
			var r *pb.ListAssetAppsResponse
			r, err = pb.NewDeploymentServiceClient(conn).ListAssetApps(ctx, &pb.ListAssetAppsRequest{AssetId: o.asset})
			if err == nil {
				out = wire(r)
			}
		case "artifact", "artifacts", "material":
			var registry *grpc.ClientConn
			registry, err = dialRegistry(auth)
			if err == nil {
				defer registry.Close()
				if o.operation == "artifact" {
					out, err = artifact(ctx, registry, o)
				} else if o.operation == "artifacts" {
					out, err = artifacts(ctx, registry, o)
				} else {
					out, err = material(ctx, registry, o)
				}
			}
		case "plan-removal":
			var d *pb.Deployment
			d, err = ownedDeployment(ctx, conn, o)
			if err == nil {
				response := new(pb.DeploymentChangeResponse)
				err = cloudrequest.Invoke(ctx, conn, auth, pb.DeploymentService_RemoveDeployment_FullMethodName, &pb.RemoveDeploymentRequest{OrganizationId: o.tenant, DeploymentId: d.Id, ExpectedRevision: d.Revision, Note: o.note}, response)
				if err == nil {
					out = map[string]any{"deployment": wire(response.Deployment), "pending": wires(response.ToSign)}
				}
			}
		case "plan-update":
			var d *pb.Deployment
			d, err = ownedDeployment(ctx, conn, o)
			if err == nil {
				values := map[string]string{}
				err = json.Unmarshal([]byte(d.ConfigJson), &values)
				if err == nil {
					values["DISPLAY_UNIT"] = "kelvin"
					data, e := json.Marshal(values)
					if e != nil {
						return e
					}
					text := string(data)
					response := new(pb.DeploymentChangeResponse)
					err = cloudrequest.Invoke(ctx, conn, auth, pb.DeploymentService_UpdateDeployment_FullMethodName,
						&pb.UpdateDeploymentRequest{OrganizationId: o.tenant, DeploymentId: d.Id, ExpectedRevision: d.Revision, ConfigJson: &text, Note: "E2E pending assignment"}, response)
					if err == nil {
						out = map[string]any{"deployment": wire(response.Deployment), "pending": wires(response.ToSign)}
					}
				}
			}
		case "unbound-create", "unbound-delete", "unbound-state", "partial-delete":
			out, err = auxiliary(ctx, conn, auth, o)
		case "stale-update":
			var d *pb.Deployment
			d, err = ownedDeployment(ctx, conn, o)
			if err == nil {
				response := new(pb.DeploymentChangeResponse)
				values := map[string]string{}
				if e := json.Unmarshal([]byte(d.ConfigJson), &values); e != nil {
					return e
				}
				values["DISPLAY_UNIT"] = "stale-attempt"
				data, e := json.Marshal(values)
				if e != nil {
					return e
				}
				text := string(data)
				err = cloudrequest.Invoke(ctx, conn, auth, pb.DeploymentService_UpdateDeployment_FullMethodName, &pb.UpdateDeploymentRequest{OrganizationId: o.tenant, DeploymentId: d.Id, ExpectedRevision: int32(o.expected), ConfigJson: &text, Note: o.note}, response)
				if err == nil {
					out = wire(response)
				}
			}
		case "fleet-prepare", "fleet-join", "fleet-restore", "fleet-state":
			out, err = fleet(ctx, conn, auth, o)
		default:
			err = errors.New("unknown public fixture operation")
		}
		return err
	})
	return out, err
}
func wire(v proto.Message) json.RawMessage {
	if v == nil || !v.ProtoReflect().IsValid() {
		return json.RawMessage("null")
	}
	b, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(v)
	if err != nil {
		panic("public proto serialization failed")
	}
	return b
}
func wires[T proto.Message](items []T) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, v := range items {
		out = append(out, wire(v))
	}
	return out
}
func pages[T any](read func(string) ([]T, string, error)) ([]T, error) {
	out := make([]T, 0)
	seen := map[string]bool{}
	token := ""
	for {
		items, next, err := read(token)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if seen[next] {
			return nil, errors.New("repeated page token")
		}
		seen[next] = true
		token = next
	}
}
func releases(ctx context.Context, c grpc.ClientConnInterface, o options) ([]*pb.AppRelease, error) {
	out := make([]*pb.AppRelease, 0)
	for offset := int32(0); ; {
		s, err := pb.NewDeploymentServiceClient(c).ListAppReleases(ctx, &pb.ListAppReleasesRequest{OrganizationId: o.tenant, AppId: o.app, Offset: offset, Limit: 2})
		if err != nil {
			return nil, err
		}
		var count, total int32
		for {
			r, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, r.AppReleases...)
			count += int32(len(r.AppReleases))
			total = r.Total
		}
		offset += count
		if count == 0 || offset >= total {
			return out, nil
		}
	}
}
func deployments(ctx context.Context, c grpc.ClientConnInterface, o options) ([]*pb.Deployment, error) {
	out := make([]*pb.Deployment, 0)
	for offset := int32(0); ; {
		s, err := pb.NewDeploymentServiceClient(c).ListDeployments(ctx, &pb.ListDeploymentsRequest{OrganizationId: o.tenant, AppId: o.app, Offset: offset, Limit: 2, IncludeRemoved: true})
		if err != nil {
			return nil, err
		}
		var count, total int32
		for {
			r, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, r.Deployments...)
			count += int32(len(r.Deployments))
			total = r.Total
		}
		offset += count
		if count == 0 || offset >= total {
			return out, nil
		}
	}
}
func catalog(ctx context.Context, c grpc.ClientConnInterface, a *config.AuthConfig, o options) (any, error) {
	app, err := pb.NewAppServiceClient(c).GetApp(ctx, &pb.GetAppRequest{Id: o.app, OrganizationId: o.tenant})
	if status.Code(err) == codes.NotFound {
		rs, releaseErr := releases(ctx, c, o)
		ds, deploymentErr := deployments(ctx, c, o)
		if releaseErr != nil && status.Code(releaseErr) != codes.NotFound {
			return nil, releaseErr
		}
		if deploymentErr != nil && status.Code(deploymentErr) != codes.NotFound {
			return nil, deploymentErr
		}
		return map[string]any{"appCode": "NotFound", "app": nil, "releases": wires(rs), "deployments": wires(ds)}, nil
	}
	if err != nil {
		return nil, err
	}
	rs, err := releases(ctx, c, o)
	if err != nil {
		return nil, err
	}
	ds, err := deployments(ctx, c, o)
	if err != nil {
		return nil, err
	}
	bs, err := pages(func(token string) ([]*pb.Binding, string, error) {
		r, e := pb.NewAccessServiceClient(c).ListBindings(ctx, &pb.ListBindingsRequest{OrganizationId: o.tenant, ScopeType: pb.BindingScopeType_BINDING_SCOPE_TYPE_APP, ScopeRef: o.app, PageSize: 2, PageToken: token})
		return r.GetBindings(), r.GetNextPageToken(), e
	})
	if err != nil {
		return nil, err
	}
	// PKI operator principals are not Cloud user ids. Resolve the authenticated
	// caller through the public self-permissions read, never through JWT claims
	// or an expected id supplied by the test.
	self, err := pb.NewAccessServiceClient(c).GetEffectivePermissions(ctx, &pb.GetEffectivePermissionsRequest{OrganizationId: o.tenant, PrincipalType: pb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER})
	if err != nil {
		return nil, err
	}
	users := map[string]bool{}
	for _, permission := range self.Permissions {
		for _, binding := range permission.Bindings {
			if binding.GetPrincipal().GetType() == pb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER && binding.GetPrincipal().GetId() != "" {
				users[binding.Principal.Id] = true
			}
		}
	}
	if len(users) != 1 {
		return nil, errors.New("self-permissions must identify one directly bound Cloud user")
	}
	var operatorID string
	for id := range users {
		operatorID = id
	}
	return map[string]any{"appCode": "OK", "app": wire(app), "operatorId": operatorID, "bindings": wires(bs), "releases": wires(rs), "deployments": wires(ds)}, nil
}
func ownedDeployment(ctx context.Context, c grpc.ClientConnInterface, o options) (*pb.Deployment, error) {
	if o.deployment != "" {
		d, err := pb.NewDeploymentServiceClient(c).GetDeployment(ctx, &pb.GetDeploymentRequest{Id: o.deployment})
		if err != nil {
			return nil, err
		}
		if d.AppId != o.app || d.OrganizationId != o.tenant {
			return nil, errors.New("deployment ownership mismatch")
		}
		return d, nil
	}
	ds, err := deployments(ctx, c, o)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.Name == o.name {
			return d, nil
		}
	}
	return nil, status.Error(codes.NotFound, "owned deployment absent")
}
func deployment(ctx context.Context, c grpc.ClientConnInterface, o options) (any, error) {
	d, err := ownedDeployment(ctx, c, o)
	if err != nil {
		return nil, err
	}
	client := pb.NewDeploymentServiceClient(c)
	rs, err := pages(func(token string) ([]*pb.DeploymentRevision, string, error) {
		r, e := client.ListDeploymentRevisions(ctx, &pb.ListDeploymentRevisionsRequest{DeploymentId: d.Id, PageSize: 2, PageToken: token})
		return r.GetRevisions(), r.GetNextPageToken(), e
	})
	if err != nil {
		return nil, err
	}
	history := make([]any, 0, len(rs))
	for _, r := range rs {
		devices, e := pages(func(token string) ([]string, string, error) {
			v, e := client.ListDeploymentRevisionDevices(ctx, &pb.ListDeploymentRevisionDevicesRequest{DeploymentId: d.Id, Revision: r.Revision, PageSize: 2, PageToken: token})
			return v.GetDeviceIds(), v.GetNextPageToken(), e
		})
		if e != nil {
			return nil, e
		}
		moves, e := pages(func(token string) ([]*pb.DeviceMove, string, error) {
			v, e := client.ListDeploymentRevisionMoves(ctx, &pb.ListDeploymentRevisionMovesRequest{DeploymentId: d.Id, Revision: r.Revision, PageSize: 2, PageToken: token})
			return v.GetMoves(), v.GetNextPageToken(), e
		})
		if e != nil {
			return nil, e
		}
		history = append(history, map[string]any{"revision": wire(r), "devices": devices, "moves": wires(moves)})
	}
	states, err := pages(func(token string) ([]*pb.AppDeviceState, string, error) {
		r, e := client.ListAppDeviceStates(ctx, &pb.ListAppDeviceStatesRequest{OrganizationId: o.tenant, AppId: o.app, DeploymentId: d.Id, PageSize: 2, PageToken: token})
		return r.GetRows(), r.GetNextPageToken(), e
	})
	if err != nil {
		return nil, err
	}
	pending, err := pages(func(token string) ([]*pb.PendingDeviceStatement, string, error) {
		r, e := client.ListPendingDeviceStatements(ctx, &pb.ListPendingDeviceStatementsRequest{DeploymentId: d.Id, PageSize: 2, PageToken: token})
		return r.GetStatements(), r.GetNextPageToken(), e
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"deployment": wire(d), "history": history, "states": wires(states), "pending": wires(pending)}, nil
}
func dialRegistry(a *config.AuthConfig) (*grpc.ClientConn, error) {
	copy := *a
	copy.CloudGRPC = "registry.dev.wendy.sh:443"
	creds, err := clouddefaults.CloudTransportCredentials(copy.CloudGRPC, &a.Certificates[0])
	if err != nil {
		return nil, err
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, cloudrequest.DPoPDialOptions(&copy, cloudsession.TokenProvider(a, heldToken))...)
	return grpc.NewClient(copy.CloudGRPC, opts...)
}
func artifact(ctx context.Context, c grpc.ClientConnInterface, o options) (any, error) {
	client := pb.NewArtifactRegistryServiceClient(c)
	r, err := client.GetArtifact(ctx, &pb.GetArtifactRequest{OrganizationId: o.tenant, AppReleaseId: o.release})
	if err != nil {
		return nil, atFixtureStage("get_artifact", err)
	}
	sum := sha256.Sum256(r.GetManifest().GetOciManifest())
	return map[string]any{"manifest": wire(r.Manifest), "computedDigest": "sha256:" + hex.EncodeToString(sum[:]), "imageName": services.ReleaseImageName(o.app, o.release)}, nil
}

func artifacts(ctx context.Context, c grpc.ClientConnInterface, o options) (any, error) {
	client := pb.NewArtifactRegistryServiceClient(c)
	list, err := pages(func(token string) ([]*pb.StoredArtifact, string, error) {
		r, e := client.ListArtifacts(ctx, &pb.ListArtifactsRequest{OrganizationId: o.tenant, AppId: o.app, PageSize: 2, PageToken: token})
		return r.GetArtifacts(), r.GetNextPageToken(), e
	})
	if err != nil {
		return nil, atFixtureStage("list_artifacts", err)
	}
	return map[string]any{"artifacts": wires(list)}, nil
}
func material(ctx context.Context, c grpc.ClientConnInterface, o options) (any, error) {
	r, err := pb.NewArtifactRegistryServiceClient(c).GetMyWraps(ctx, &pb.GetMyWrapsRequest{OrganizationId: o.tenant, Scope: &pb.WrapScope{Scope: &pb.WrapScope_DeploymentId{DeploymentId: o.deployment}}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	sealed := make([]string, 0)
	for _, w := range r.Wraps {
		ids = append(ids, w.GetScope().GetKeyId())
	}
	for _, s := range r.SealedContentKeys {
		sealed = append(sealed, s.DeploymentKeyId)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	slices.Sort(sealed)
	sealed = slices.Compact(sealed)
	return map[string]any{"keyIds": ids, "sealedKeyIds": sealed}, nil
}
