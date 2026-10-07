package commands

import (
	"context"
	"errors"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
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
		return &v2AssetState{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: unenrollDevice, DeletedAt: timestamppb.New(time.Now())}}
	}
	return &v2AssetState{Active: testUnenrollAsset()}
}
func testRevokeAck(j v2UnenrollProgress) *agentpbv2.RevokeACMECertificateResponse {
	return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: j.Principal, CertificateSha256: j.Fingerprint, CertificateSerial: "2a"}
}
func TestV2UnenrollPhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "revocation", "bad-ack", "binding-recheck", "delete", "confirm-deletion", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			reads := 0
			deleted := false
			resets := 0
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*v2AssetState, error) {
					reads++
					if (reads == 1 && failure == "initial-read") || (reads == 2 && failure == "binding-recheck") || (reads == 3 && failure == "confirm-deletion") {
						return nil, errors.New("uncertain")
					}
					return testLifecycle(deleted), nil
				},
				revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					if failure == "revocation" {
						return nil, errors.New("uncertain")
					}
					a := testRevokeAck(j)
					if failure == "bad-ack" {
						a.CertificateSha256 = strings.Repeat("cd", 32)
					}
					return a, nil
				},
				delete: func(context.Context, string) error {
					if failure == "delete" {
						return errors.New("uncertain")
					}
					deleted = true
					return nil
				},
				reset: func(context.Context, string, string) error {
					resets++
					if failure == "reset" {
						return errors.New("uncertain")
					}
					return nil
				},
			}
			err := performV2Unenroll(context.Background(), &j, ops)
			if (err != nil) != (failure != "") {
				t.Fatalf("error: %v", err)
			}
			wantRevoked := failure != "initial-read" && failure != "revocation" && failure != "bad-ack"
			wantDeleted := failure == "" || failure == "reset"
			if j.Revoked != wantRevoked || j.Deleted != wantDeleted || j.Reset != (failure == "") || (resets > 0) != wantDeleted {
				t.Fatalf("wrong progress: %+v", j)
			}
		})
	}
}
func TestV2UnenrollStatelessResumeAfterLostDeleteResponse(t *testing.T) {
	deleted := false
	deletions := 0
	resets := 0
	ops := v2UnenrollOps{
		lookup: func(context.Context, string) (*v2AssetState, error) { return testLifecycle(deleted), nil },
		revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
			return testRevokeAck(testUnenrollProgress()), nil
		},
		delete: func(context.Context, string) error { deletions++; deleted = true; return errors.New("lost response") },
		reset:  func(context.Context, string, string) error { resets++; return nil },
	}
	first := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &first, ops); err == nil || resets != 0 {
		t.Fatal("uncertain deletion reset")
	}
	retry := testUnenrollProgress()
	retry.AssetID = ""
	if err := performV2Unenroll(context.Background(), &retry, ops); err != nil {
		t.Fatal(err)
	}
	if !retry.Reset || retry.AssetID != unenrollAsset || deletions != 1 || resets != 1 {
		t.Fatal("retry did not converge")
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
	deleted, err := v2UnenrollLifecycle(result, &j)
	if err != nil || !deleted {
		t.Fatal("typed deletion evidence not recognized")
	}
	for _, e := range []error{status.Error(codes.NotFound, "asset deleted"), status.Error(codes.InvalidArgument, "old server requires id"), status.Error(codes.PermissionDenied, "denied")} {
		if _, err := v2AssetLookupResult(nil, e); err == nil {
			t.Fatal("status/message alone became deletion proof")
		}
	}
	wrong, _ := status.New(codes.FailedPrecondition, "wrong status").WithDetails(d)
	if _, err := v2AssetLookupResult(nil, wrong.Err()); err == nil {
		t.Fatal("accepted deletion detail on wrong status")
	}
	multiple, _ := status.New(codes.NotFound, "ambiguous").WithDetails(d, d)
	if _, err := v2AssetLookupResult(nil, multiple.Err()); err == nil {
		t.Fatal("ambiguous proof accepted")
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
						r.Deleted.PkiDeviceName = "other"
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
					t.Fatal("invalid evidence accepted")
				}
			})
		}
	}
}
func TestV2UnenrollRechecksBindingAndDeletionEvidence(t *testing.T) {
	for _, failure := range []string{"rebound", "purged", "still-active"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			reads := 0
			resets := 0
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*v2AssetState, error) {
					reads++
					r := testLifecycle(false)
					if reads == 2 && failure == "rebound" {
						r.Active.PkiDeviceName = proto.String("other")
					}
					if reads == 3 && failure == "purged" {
						r = &v2AssetState{}
					}
					return r, nil
				},
				revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					return testRevokeAck(j), nil
				},
				delete: func(context.Context, string) error { return nil }, reset: func(context.Context, string, string) error { resets++; return nil },
			}
			if err := performV2Unenroll(context.Background(), &j, ops); err == nil || resets != 0 {
				t.Fatal("reset without current proof")
			}
		})
	}
}
