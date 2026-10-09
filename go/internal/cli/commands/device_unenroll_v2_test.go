package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
func TestV2UnenrollCloudOwnedPhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "delete", "confirm-deletion", "proof", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			reads, resets := 0, 0
			deleted := false
			order := []string{}
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*v2AssetState, error) {
					reads++
					if (reads == 1 && failure == "initial-read") || (reads == 2 && failure == "confirm-deletion") {
						return nil, errors.New("uncertain")
					}
					return testLifecycle(deleted), nil
				},
				delete: func(context.Context, string) error {
					order = append(order, "cloud")
					if failure == "delete" {
						return errors.New("uncertain")
					}
					deleted = true
					return nil
				},
				proof: func(context.Context) ([]byte, error) {
					order = append(order, "verify-PKI")
					if failure == "proof" {
						return nil, errors.New("unconfirmed")
					}
					return []byte("verified"), nil
				},
				reset: func(_ context.Context, p, fp string, evidence []byte, d *cloudpbv2.DeletedAsset) error {
					order = append(order, "reset")
					resets++
					if p != j.Principal || fp != j.Fingerprint || string(evidence) != "verified" || d.GetId() != j.AssetID {
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
			wantDeleted := failure == "" || failure == "proof" || failure == "reset"
			wantRevoked := failure == "" || failure == "reset"
			if j.Deleted != wantDeleted || j.Revoked != wantRevoked || j.Reset != (failure == "") || (resets > 0) != wantRevoked {
				t.Fatalf("false progress: %+v", j)
			}
			if failure == "" && strings.Join(order, ",") != "cloud,verify-PKI,reset" {
				t.Fatal(order)
			}
		})
	}
}
func TestV2UnenrollStatelessResumeAfterLostDeleteResponse(t *testing.T) {
	deleted := false
	deletions, resets := 0, 0
	ops := v2UnenrollOps{lookup: func(context.Context, string) (*v2AssetState, error) { return testLifecycle(deleted), nil }, delete: func(context.Context, string) error { deletions++; deleted = true; return errors.New("reply lost") }, proof: func(context.Context) ([]byte, error) { return []byte("proof"), nil }, reset: func(context.Context, string, string, []byte, *cloudpbv2.DeletedAsset) error { resets++; return nil }}
	first := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &first, ops); err == nil || resets != 0 {
		t.Fatal("uncertain delete allowed erasure")
	}
	retry := testUnenrollProgress()
	retry.AssetID = ""
	if err := performV2Unenroll(context.Background(), &retry, ops); err != nil {
		t.Fatal(err)
	}
	if !retry.Reset || retry.AssetID != unenrollAsset || deletions != 1 || resets != 1 {
		t.Fatal("stateless retry failed")
	}
}
func TestV2UnenrollDeletedErrorDetails(t *testing.T) {
	d := testLifecycle(true).Deleted
	s, err := status.New(codes.NotFound, "arbitrary message").WithDetails(d)
	if err != nil {
		t.Fatal(err)
	}
	result, err := v2AssetLookupResult(nil, s.Err())
	if err != nil {
		t.Fatal(err)
	}
	j := testUnenrollProgress()
	if deleted, err := v2UnenrollLifecycle(result, &j); err != nil || !deleted {
		t.Fatal("typed evidence lost")
	}
	for _, e := range []error{status.Error(codes.NotFound, "deleted"), status.Error(codes.InvalidArgument, "old server requires id"), status.Error(codes.PermissionDenied, "denied")} {
		if _, err := v2AssetLookupResult(nil, e); err == nil {
			t.Fatal("status alone accepted")
		}
	}
	wrong, _ := status.New(codes.FailedPrecondition, "wrong").WithDetails(d)
	if _, err := v2AssetLookupResult(nil, wrong.Err()); err == nil {
		t.Fatal("wrong status accepted")
	}
	multiple, _ := status.New(codes.NotFound, "ambiguous").WithDetails(d, d)
	if _, err := v2AssetLookupResult(nil, multiple.Err()); err == nil {
		t.Fatal("ambiguous evidence accepted")
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
func TestV2UnenrollRechecksBindingAndDeletionEvidence(t *testing.T) {
	for _, failure := range []string{"rebound", "purged", "still-active"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			reads, resets := 0, 0
			ops := v2UnenrollOps{lookup: func(context.Context, string) (*v2AssetState, error) {
				reads++
				r := testLifecycle(false)
				if reads == 2 {
					switch failure {
					case "rebound":
						r = testLifecycle(true)
						r.Deleted.DeviceId = "other"
					case "purged":
						r = &v2AssetState{}
					}
				}
				return r, nil
			}, delete: func(context.Context, string) error { return nil }, proof: func(context.Context) ([]byte, error) { t.Fatal("proof check after bad Cloud binding"); return nil, nil }, reset: func(context.Context, string, string, []byte, *cloudpbv2.DeletedAsset) error { resets++; return nil }}
			if err := performV2Unenroll(context.Background(), &j, ops); err == nil || resets != 0 {
				t.Fatal("reset without authoritative proof")
			}
		})
	}
}
