package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
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
func testLifecycle(deleted bool) *cloudpbv2.GetAssetLifecycleResponse {
	if deleted {
		return &cloudpbv2.GetAssetLifecycleResponse{State: &cloudpbv2.GetAssetLifecycleResponse_Deleted{Deleted: &cloudpbv2.DeletedAsset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: unenrollDevice, DeletedAt: timestamppb.New(time.Now())}}}
	}
	return &cloudpbv2.GetAssetLifecycleResponse{State: &cloudpbv2.GetAssetLifecycleResponse_Active{Active: testUnenrollAsset()}}
}
func testRevokeAck(j v2UnenrollProgress) *agentpbv2.RevokeACMECertificateResponse {
	return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: j.Principal, CertificateSha256: j.Fingerprint, CertificateSerial: "2a"}
}

func TestV2UnenrollPhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "revocation", "bad-ack", "binding-recheck", "delete", "confirm-deletion", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollProgress()
			var calls []string
			reads := 0
			deleted := false
			fail := errors.New("uncertain")
			ops := v2UnenrollOps{
				lookup: func(context.Context, string) (*cloudpbv2.GetAssetLifecycleResponse, error) {
					calls = append(calls, "lookup")
					reads++
					if (reads == 1 && failure == "initial-read") || (reads == 2 && failure == "binding-recheck") || (reads == 3 && failure == "confirm-deletion") {
						return nil, fail
					}
					return testLifecycle(deleted), nil
				},
				revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					calls = append(calls, "revoke")
					if failure == "revocation" {
						return nil, fail
					}
					ack := testRevokeAck(j)
					if failure == "bad-ack" {
						ack.CertificateSha256 = strings.Repeat("cd", 32)
					}
					return ack, nil
				},
				delete: func(context.Context, string) error {
					calls = append(calls, "delete")
					if failure == "delete" {
						return fail
					}
					deleted = true
					return nil
				},
				reset: func(context.Context, string, string) error {
					calls = append(calls, "reset")
					if failure == "reset" {
						return fail
					}
					return nil
				},
			}
			err := performV2Unenroll(context.Background(), &j, ops)
			if (err != nil) != (failure != "") {
				t.Fatalf("error = %v", err)
			}
			wantRevoked := failure != "initial-read" && failure != "revocation" && failure != "bad-ack"
			wantDeleted := failure == "" || failure == "reset"
			if j.Revoked != wantRevoked || j.Deleted != wantDeleted || j.Reset != (failure == "") {
				t.Fatalf("incorrect confirmation flags: %+v", j)
			}
			if failure == "" && !reflect.DeepEqual(calls, []string{"lookup", "revoke", "lookup", "delete", "lookup", "reset"}) {
				t.Fatalf("wrong phase order: %v", calls)
			}
		})
	}
}

func TestV2UnenrollStatelessResumeAfterLostDeleteResponse(t *testing.T) {
	deleted := false
	revoked := false
	revocations := 0
	deletions := 0
	resets := 0
	ops := v2UnenrollOps{
		lookup: func(context.Context, string) (*cloudpbv2.GetAssetLifecycleResponse, error) {
			return testLifecycle(deleted), nil
		},
		revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
			if !revoked {
				revocations++
				revoked = true
			}
			return testRevokeAck(testUnenrollProgress()), nil
		},
		delete: func(context.Context, string) error { deletions++; deleted = true; return errors.New("response lost") },
		reset:  func(context.Context, string, string) error { resets++; return nil },
	}
	first := testUnenrollProgress()
	if err := performV2Unenroll(context.Background(), &first, ops); err == nil || first.Deleted || resets != 0 {
		t.Fatal("uncertain deletion reset the device")
	}
	// Fresh CLI invocation: no serialized progress survives. Agent/Cloud evidence does.
	retry := testUnenrollProgress()
	retry.AssetID = ""
	if err := performV2Unenroll(context.Background(), &retry, ops); err != nil {
		t.Fatal(err)
	}
	if !retry.Reset || retry.AssetID != unenrollAsset || revocations != 1 || deletions != 1 || resets != 1 {
		t.Fatalf("non-convergent retry: %+v, %d/%d/%d", retry, revocations, deletions, resets)
	}
}

func TestV2UnenrollLifecycleRejectsUnknownOrChangedBinding(t *testing.T) {
	for _, bad := range []string{"unknown", "nil", "tenant", "device", "asset", "missing-time"} {
		for _, deleted := range []bool{false, true} {
			t.Run(bad+map[bool]string{true: "/deleted", false: "/active"}[deleted], func(t *testing.T) {
				j := testUnenrollProgress()
				reply := testLifecycle(deleted)
				switch bad {
				case "nil":
					reply = nil
				case "unknown":
					reply = &cloudpbv2.GetAssetLifecycleResponse{State: &cloudpbv2.GetAssetLifecycleResponse_Unknown{Unknown: true}}
				case "tenant":
					if deleted {
						reply.GetDeleted().OrganizationId = "other"
					} else {
						reply.GetActive().OrganizationId = "other"
					}
				case "device":
					if deleted {
						reply.GetDeleted().PkiDeviceName = "other"
					} else {
						reply.GetActive().PkiDeviceName = proto.String("other")
					}
				case "asset":
					if deleted {
						reply.GetDeleted().Id = "44444444-4444-4444-8444-444444444444"
					} else {
						reply.GetActive().Id = "44444444-4444-4444-8444-444444444444"
					}
				case "missing-time":
					if !deleted {
						return
					}
					reply.GetDeleted().DeletedAt = nil
				}
				if _, err := v2UnenrollLifecycle(reply, &j); err == nil {
					t.Fatal("invalid lifecycle evidence accepted")
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
				lookup: func(context.Context, string) (*cloudpbv2.GetAssetLifecycleResponse, error) {
					reads++
					reply := testLifecycle(false)
					if reads == 2 && failure == "rebound" {
						reply.GetActive().PkiDeviceName = proto.String("other")
					}
					if reads == 3 && failure == "purged" {
						reply = &cloudpbv2.GetAssetLifecycleResponse{State: &cloudpbv2.GetAssetLifecycleResponse_Unknown{Unknown: true}}
					}
					return reply, nil
				},
				revoke: func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					return testRevokeAck(j), nil
				},
				delete: func(context.Context, string) error { return nil },
				reset:  func(context.Context, string, string) error { resets++; return nil },
			}
			if err := performV2Unenroll(context.Background(), &j, ops); err == nil || resets != 0 {
				t.Fatal("reset without current tombstone binding")
			}
		})
	}
}
