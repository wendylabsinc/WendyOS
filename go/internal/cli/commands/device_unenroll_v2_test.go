package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const unenrollTenant = "11111111-1111-4111-8111-111111111111"
const unenrollAsset = "22222222-2222-4222-8222-222222222222"
const unenrollDevice = "33333333-3333-4333-8333-333333333333"

func testUnenrollProgress() v2UnenrollProgress {
	return v2UnenrollProgress{Cloud: "api.example:443", Principal: "spiffe://wendy.sh/tenant/" + unenrollTenant + "/device/" + unenrollDevice, AssetID: unenrollAsset, Fingerprint: strings.Repeat("ab", 32)}
}
func testUnenrollAsset() *cloudpbv2.Asset {
	return &cloudpbv2.Asset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: proto.String(unenrollDevice)}
}
func testLifecycle(deleted bool) *v2AssetState {
	if deleted {
		return &v2AssetState{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollAsset, OrganizationId: unenrollTenant, DeviceId: unenrollDevice, DeletedAt: timestamppb.New(time.Now())}}
	}
	return &v2AssetState{Active: testUnenrollAsset()}
}
func testAuthorizedReset(t *testing.T) *v2AuthorizedReset {
	t.Helper()
	deleted := testLifecycle(true).Deleted
	deleted.DeletedAt = timestamppb.New(time.Unix(1700000000, 0))
	raw, err := proto.Marshal(deleted)
	if err != nil {
		t.Fatal(err)
	}
	return &v2AuthorizedReset{deletion: raw, token: []byte("verified-by-authorize")}
}

func TestV2UnenrollCloudOwnedSignedResponsePhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "authorize", "proof", "refresh", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			calls, resets := 0, 0
			order := []string{}
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*v2AssetState, error) {
					if failure == "initial-read" {
						return nil, errors.New("unavailable")
					}
					return testLifecycle(false), nil
				},
				authorize: func(context.Context, string) (*v2AuthorizedReset, error) {
					calls++
					order = append(order, "authorize")
					if (calls == 1 && failure == "authorize") || (calls == 2 && failure == "refresh") {
						return nil, errors.New("uncertain")
					}
					return testAuthorizedReset(t), nil
				},
				proof: func(context.Context) ([]byte, error) {
					order = append(order, "verify-PKI")
					if failure == "proof" {
						return nil, errors.New("unconfirmed")
					}
					return []byte("verified"), nil
				},
				reset: func(_ context.Context, p, fp string, evidence []byte, a *v2AuthorizedReset) error {
					resets++
					order = append(order, "reset")
					if p != j.Principal || fp != j.Fingerprint || string(evidence) != "verified" || a == nil {
						t.Fatal("binding lost")
					}
					if failure == "reset" {
						return errors.New("uncertain")
					}
					return nil
				},
			}
			err := performV2Unenroll(context.Background(), &j, ops)
			if (err != nil) != (failure != "") {
				t.Fatal(err)
			}
			wantDeleted := failure == "" || failure == "proof" || failure == "refresh" || failure == "reset"
			wantRevoked := failure == "" || failure == "refresh" || failure == "reset"
			if j.Deleted != wantDeleted || j.Revoked != wantRevoked || j.Reset != (failure == "") || (resets > 0) != (failure == "" || failure == "reset") {
				t.Fatalf("false progress: %+v", j)
			}
			if failure == "" && strings.Join(order, ",") != "authorize,verify-PKI,authorize,reset" {
				t.Fatal(order)
			}
		})
	}
}

func TestV2UnenrollStatelessResumeAfterLostDeleteResponse(t *testing.T) {
	gone := false
	deletions, resets, authorizations := 0, 0, 0
	ops := v2UnenrollOps{
		lookup: func(context.Context, string) (*v2AssetState, error) {
			if gone {
				return nil, status.Error(codes.NotFound, "asset not found")
			}
			return testLifecycle(false), nil
		},
		authorize: func(context.Context, string) (*v2AuthorizedReset, error) {
			authorizations++
			if !gone {
				gone = true
				deletions++
				return nil, errors.New("reply lost")
			}
			return testAuthorizedReset(t), nil
		},
		proof: func(context.Context) ([]byte, error) { return []byte("proof"), nil },
		reset: func(context.Context, string, string, []byte, *v2AuthorizedReset) error { resets++; return nil },
	}
	first := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &first, ops); err == nil || resets != 0 {
		t.Fatal("uncertain delete allowed erasure")
	}
	// A plain NotFound cannot discover a former Cloud UUID. Require the explicit
	// asset ID, then obtain fresh signed audit evidence rather than trust NotFound.
	missing := testUnenrollProgress()
	missing.AssetID = ""
	if err := performV2Unenroll(context.Background(), &missing, ops); err == nil || authorizations != 1 {
		t.Fatal("unknown deleted asset accepted")
	}
	retry := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &retry, ops); err != nil {
		t.Fatal(err)
	}
	if !retry.Reset || deletions != 1 || resets != 1 || authorizations != 3 {
		t.Fatal("exact-asset stateless retry failed")
	}
}

func TestV2UnenrollSignedEvidenceRequiredAfterNotFound(t *testing.T) {
	for _, bad := range []string{"missing", "token", "deletion", "wrong-asset", "changed", "unavailable", "denied"} {
		t.Run(bad, func(t *testing.T) {
			j := testUnenrollProgress()
			calls, resets := 0, 0
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*v2AssetState, error) {
					code := codes.NotFound
					if bad == "unavailable" {
						code = codes.Unavailable
					}
					if bad == "denied" {
						code = codes.PermissionDenied
					}
					return nil, status.Error(code, "absent")
				},
				authorize: func(context.Context, string) (*v2AuthorizedReset, error) {
					calls++
					a := testAuthorizedReset(t)
					switch bad {
					case "missing":
						return nil, nil
					case "token":
						a.token = nil
					case "deletion":
						a.deletion = []byte("invalid")
					case "wrong-asset":
						d := testLifecycle(true).Deleted
						d.Id = "44444444-4444-4444-8444-444444444444"
						a.deletion, _ = proto.Marshal(d)
					case "changed":
						if calls == 2 {
							d := testLifecycle(true).Deleted
							d.DeletedAt = timestamppb.New(time.Now().Add(-time.Hour))
							a.deletion, _ = proto.Marshal(d)
						}
					}
					return a, nil
				},
				proof: func(context.Context) ([]byte, error) { return []byte("proof"), nil },
				reset: func(context.Context, string, string, []byte, *v2AuthorizedReset) error { resets++; return nil },
			}
			if err := performV2Unenroll(context.Background(), &j, ops); err == nil || resets != 0 {
				t.Fatal("unconfirmed authorization allowed reset")
			}
			if (bad == "unavailable" || bad == "denied") && calls != 0 {
				t.Fatal("Cloud refusal bypassed")
			}
		})
	}
}

func TestV2UnenrollLifecycleRejectsUnknownOrChangedBinding(t *testing.T) {
	for _, bad := range []string{"unknown", "nil", "tenant", "device", "asset", "missing-time"} {
		for _, deleted := range []bool{false, true} {
			t.Run(bad+map[bool]string{true: "/deleted", false: "/active"}[deleted], func(t *testing.T) {
				j := testUnenrollProgress()
				r := testLifecycle(deleted)
				switch bad {
				case "nil":
					r = nil
				case "unknown":
					r = &v2AssetState{}
				case "tenant":
					if deleted {
						r.Deleted.OrganizationId = "other"
					} else {
						r.Active.OrganizationId = "other"
					}
				case "device":
					if deleted {
						r.Deleted.DeviceId = "other"
					} else {
						r.Active.PkiDeviceName = proto.String("other")
					}
				case "asset":
					if deleted {
						r.Deleted.Id = "44444444-4444-4444-8444-444444444444"
					} else {
						r.Active.Id = "44444444-4444-4444-8444-444444444444"
					}
				case "missing-time":
					if !deleted {
						return
					}
					r.Deleted.DeletedAt = nil
				}
				if _, err := v2UnenrollLifecycle(r, &j); err == nil {
					t.Fatal("invalid binding accepted")
				}
			})
		}
	}
}
func TestV2UnenrollUnauthenticatedAgentRefusedBeforeCloudAccess(t *testing.T) {
	// The prerelease CloudUnenrollmentSupported flag alone never qualifies,
	// not even for --check.
	for _, advertised := range []bool{false, true} {
		for _, flags := range []struct {
			name           string
			yes, checkOnly bool
		}{
			{name: "interactive"},
			{name: "yes", yes: true},
			{name: "check", checkOnly: true},
		} {
			t.Run(fmt.Sprintf("legacy-flag=%t/%s", advertised, flags.name), func(t *testing.T) {
				prov := &agentpbv2.ProvisionedResponse{
					CloudHost:                  testUnenrollProgress().Cloud,
					PrincipalUri:               testUnenrollProgress().Principal,
					CloudUnenrollmentSupported: advertised,
				}
				// Missing connection/auth configuration would fail if the gate
				// let the Cloud workflow start.
				err := runV2DeviceUnenroll(context.Background(), nil, prov, nil, "", "", flags.yes, flags.checkOnly, false)
				if err == nil || !strings.Contains(err.Error(), "does not support Cloud-authenticated reset") || !strings.Contains(err.Error(), "no mutation performed") {
					t.Fatalf("unsafe workflow was not rejected before preflight: %v", err)
				}
			})
		}
	}
}

func TestV2UnenrollAuthenticatedResetBitAloneQualifies(t *testing.T) {
	for _, checkOnly := range []bool{false, true} {
		prov := &agentpbv2.ProvisionedResponse{
			CloudHost:                        testUnenrollProgress().Cloud,
			PrincipalUri:                     testUnenrollProgress().Principal,
			CloudAuthenticatedResetSupported: true,
		}
		// Passes the capability gate, then stops at the direct-mTLS peer check.
		err := runV2DeviceUnenroll(context.Background(), nil, prov, nil, "", "", true, checkOnly, false)
		if err == nil || !strings.Contains(err.Error(), "directly verified mTLS device connection") {
			t.Fatalf("check=%t: want direct-mTLS refusal after the capability gate, got %v", checkOnly, err)
		}
	}
}

// Each mode has its own capability: local-reset support never admits the
// Cloud flow, and Cloud-authenticated reset support never admits --force.
func TestV2UnenrollModesRequireTheirOwnCapability(t *testing.T) {
	for _, tc := range []struct {
		name                string
		cloud, local, force bool
		want                string
	}{
		{name: "local only, Cloud flow", local: true, want: "does not support Cloud-authenticated reset"},
		{name: "Cloud only, forced", cloud: true, force: true, want: "does not support forced local enrollment reset"},
		{name: "local, forced", local: true, force: true, want: "directly verified mTLS device connection"},
	} {
		prov := &agentpbv2.ProvisionedResponse{
			CloudHost:                        testUnenrollProgress().Cloud,
			PrincipalUri:                     testUnenrollProgress().Principal,
			CloudAuthenticatedResetSupported: tc.cloud,
			LocalResetSupported:              tc.local,
		}
		err := runV2DeviceUnenroll(context.Background(), nil, prov, nil, "", "", true, false, tc.force)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want %q before Cloud access, got %v", tc.name, tc.want, err)
		}
	}
}

func TestV2UnenrollRequiresDeviceConnectionOperatorLeaf(t *testing.T) {
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PemCertificate: "renewed"}}}
	for _, conn := range []*grpcclient.AgentConnection{{}, {CertInfo: &config.CertificateInfo{PemCertificate: "dialed"}}} {
		if err := requireDeviceOperatorLeaf(conn, auth); err == nil || !strings.Contains(err.Error(), "no mutation performed") {
			t.Fatalf("mismatched operator leaf accepted: %v", err)
		}
	}
	if err := requireDeviceOperatorLeaf(&grpcclient.AgentConnection{CertInfo: &config.CertificateInfo{PemCertificate: "renewed"}}, auth); err != nil {
		t.Fatal(err)
	}
}

// Before cloud #873 the device UUID is pki_device_name on a random asset id;
// after it, the UUID is the asset id and pki_device_name is empty.
func TestV2UnenrollBindingMatchesBothAssetShapes(t *testing.T) {
	postProgress := testUnenrollProgress()
	postProgress.AssetID = unenrollDevice
	for name, tc := range map[string]struct {
		asset    *cloudpbv2.Asset
		progress v2UnenrollProgress
	}{
		"pre-873":  {testUnenrollAsset(), testUnenrollProgress()},
		"post-873": {&cloudpbv2.Asset{Id: unenrollDevice, OrganizationId: unenrollTenant}, postProgress},
	} {
		if err := checkV2UnenrollBinding(tc.asset, tc.progress); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Deletion evidence without a device id falls back to the asset id too.
	reply := &v2AssetState{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollDevice, OrganizationId: unenrollTenant, DeletedAt: timestamppb.New(time.Now())}}
	progress := postProgress
	if _, err := v2UnenrollLifecycle(reply, &progress); err != nil {
		t.Errorf("post-873 deletion evidence: %v", err)
	}
}
