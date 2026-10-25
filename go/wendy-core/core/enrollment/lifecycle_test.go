package enrollment

import (
	"context"
	"errors"
	cloudpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

const unenrollTenant = "11111111-1111-4111-8111-111111111111"
const unenrollAsset = "22222222-2222-4222-8222-222222222222"
const unenrollDevice = "33333333-3333-4333-8333-333333333333"

func testUnenrollProgress() Progress {
	return Progress{Cloud: "api.example:443", Principal: "spiffe://wendy.sh/tenant/" + unenrollTenant + "/device/" + unenrollDevice, AssetID: unenrollAsset, Fingerprint: strings.Repeat("ab", 32)}
}
func testUnenrollAsset() *cloudpbv2.Asset {
	return &cloudpbv2.Asset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: proto.String(unenrollDevice)}
}
func testLifecycle(deleted bool) *AssetState {
	if deleted {
		return &AssetState{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollAsset, OrganizationId: unenrollTenant, DeviceId: unenrollDevice, DeletedAt: timestamppb.New(time.Now())}}
	}
	return &AssetState{Active: testUnenrollAsset()}
}
func testAuthorizedReset(t *testing.T) *AuthorizedReset {
	t.Helper()
	deleted := testLifecycle(true).Deleted
	deleted.DeletedAt = timestamppb.New(time.Unix(1700000000, 0))
	raw, err := proto.Marshal(deleted)
	if err != nil {
		t.Fatal(err)
	}
	return &AuthorizedReset{Deletion: raw, Token: []byte("verified-by-authorize")}
}

func TestV2UnenrollCloudOwnedSignedResponsePhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "authorize", "proof", "refresh", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			calls, resets := 0, 0
			order := []string{}
			ops := UnenrollOps{
				Lookup: func(context.Context, string) (*AssetState, error) {
					if failure == "initial-read" {
						return nil, errors.New("unavailable")
					}
					return testLifecycle(false), nil
				},
				Authorize: func(context.Context, string) (*AuthorizedReset, error) {
					calls++
					order = append(order, "authorize")
					if (calls == 1 && failure == "authorize") || (calls == 2 && failure == "refresh") {
						return nil, errors.New("uncertain")
					}
					return testAuthorizedReset(t), nil
				},
				Proof: func(context.Context) ([]byte, error) {
					order = append(order, "verify-PKI")
					if failure == "proof" {
						return nil, errors.New("unconfirmed")
					}
					return []byte("verified"), nil
				},
				Reset: func(_ context.Context, p, fp string, evidence []byte, a *AuthorizedReset) error {
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
			err := PerformUnenroll(context.Background(), &j, ops)
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
	ops := UnenrollOps{
		Lookup: func(context.Context, string) (*AssetState, error) {
			if gone {
				return nil, &platform.RPCStatusError{Code: platform.RPCNotFound, Message: "asset not found"}
			}
			return testLifecycle(false), nil
		},
		Authorize: func(context.Context, string) (*AuthorizedReset, error) {
			authorizations++
			if !gone {
				gone = true
				deletions++
				return nil, errors.New("reply lost")
			}
			return testAuthorizedReset(t), nil
		},
		Proof: func(context.Context) ([]byte, error) { return []byte("proof"), nil },
		Reset: func(context.Context, string, string, []byte, *AuthorizedReset) error { resets++; return nil },
	}
	first := testUnenrollProgress()
	if err := PerformUnenroll(context.Background(), &first, ops); err == nil || resets != 0 {
		t.Fatal("uncertain delete allowed erasure")
	}
	// A plain NotFound cannot discover a former Cloud UUID. Require the explicit
	// asset ID, then obtain fresh signed audit evidence rather than trust NotFound.
	missing := testUnenrollProgress()
	missing.AssetID = ""
	if err := PerformUnenroll(context.Background(), &missing, ops); err == nil || authorizations != 1 {
		t.Fatal("unknown deleted asset accepted")
	}
	retry := testUnenrollProgress()
	if err := PerformUnenroll(context.Background(), &retry, ops); err != nil {
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
			ops := UnenrollOps{
				Lookup: func(context.Context, string) (*AssetState, error) {
					code := platform.RPCNotFound
					if bad == "unavailable" {
						code = platform.RPCUnavailable
					}
					if bad == "denied" {
						code = platform.RPCPermissionDenied
					}
					return nil, &platform.RPCStatusError{Code: code, Message: "absent"}
				},
				Authorize: func(context.Context, string) (*AuthorizedReset, error) {
					calls++
					a := testAuthorizedReset(t)
					switch bad {
					case "missing":
						return nil, nil
					case "token":
						a.Token = nil
					case "deletion":
						a.Deletion = []byte("invalid")
					case "wrong-asset":
						d := testLifecycle(true).Deleted
						d.Id = "44444444-4444-4444-8444-444444444444"
						a.Deletion, _ = proto.Marshal(d)
					case "changed":
						if calls == 2 {
							d := testLifecycle(true).Deleted
							d.DeletedAt = timestamppb.New(time.Now().Add(-time.Hour))
							a.Deletion, _ = proto.Marshal(d)
						}
					}
					return a, nil
				},
				Proof: func(context.Context) ([]byte, error) { return []byte("proof"), nil },
				Reset: func(context.Context, string, string, []byte, *AuthorizedReset) error { resets++; return nil },
			}
			if err := PerformUnenroll(context.Background(), &j, ops); err == nil || resets != 0 {
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
					r = &AssetState{}
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
				if _, err := UnenrollLifecycle(r, &j); err == nil {
					t.Fatal("invalid binding accepted")
				}
			})
		}
	}
}
func TestV2UnenrollBindingMatchesBothAssetShapes(t *testing.T) {
	postProgress := testUnenrollProgress()
	postProgress.AssetID = unenrollDevice
	for name, tc := range map[string]struct {
		asset    *cloudpbv2.Asset
		progress Progress
	}{
		"pre-873":  {testUnenrollAsset(), testUnenrollProgress()},
		"post-873": {&cloudpbv2.Asset{Id: unenrollDevice, OrganizationId: unenrollTenant}, postProgress},
	} {
		if err := CheckUnenrollBinding(tc.asset, tc.progress); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Deletion evidence without a device id falls back to the asset id too.
	reply := &AssetState{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollDevice, OrganizationId: unenrollTenant, DeletedAt: timestamppb.New(time.Now())}}
	progress := postProgress
	if _, err := UnenrollLifecycle(reply, &progress); err != nil {
		t.Errorf("post-873 deletion evidence: %v", err)
	}
}
