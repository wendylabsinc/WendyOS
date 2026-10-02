package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const unenrollTenant = "11111111-1111-4111-8111-111111111111"
const unenrollAsset = "22222222-2222-4222-8222-222222222222"
const unenrollDevice = "33333333-3333-4333-8333-333333333333"

func testUnenrollJournal() v2UnenrollJournal {
	return v2UnenrollJournal{Cloud: "api.example:443", Principal: "spiffe://wendy.sh/tenant/" + unenrollTenant + "/device/" + unenrollDevice, AssetID: unenrollAsset, Fingerprint: strings.Repeat("ab", 32)}
}
func testUnenrollAsset() *cloudpbv2.Asset {
	return &cloudpbv2.Asset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: proto.String(unenrollDevice)}
}

func TestV2UnenrollPhases(t *testing.T) {
	for _, failure := range []string{"", "initial-read", "revocation", "binding-recheck", "delete", "save-after-revoke", "save-after-delete", "reset"} {
		t.Run(failure, func(t *testing.T) {
			j := testUnenrollJournal()
			var calls []string
			reads := 0
			boom := errors.New("injected failure")
			ops := v2UnenrollOps{
				get: func(_ context.Context, id string) (*cloudpbv2.Asset, error) {
					if id != unenrollAsset {
						t.Fatal("wrong UUID")
					}
					reads++
					calls = append(calls, "read")
					if reads == 1 && failure == "initial-read" {
						return nil, boom
					}
					if reads == 2 && failure == "binding-recheck" {
						return &cloudpbv2.Asset{Id: unenrollAsset, OrganizationId: unenrollTenant, PkiDeviceName: proto.String("other-device")}, nil
					}
					return testUnenrollAsset(), nil
				},
				revoke: func(_ context.Context, p, f string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					calls = append(calls, "revoke")
					if p != j.Principal || f != j.Fingerprint {
						t.Fatal("wrong installed certificate")
					}
					if failure == "revocation" {
						return nil, boom
					}
					return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: p, CertificateSha256: f, CertificateSerial: "42"}, nil
				},
				delete: func(_ context.Context, id string) error {
					calls = append(calls, "delete")
					if id != unenrollAsset {
						t.Fatal("wrong UUID")
					}
					if failure == "delete" {
						return boom
					}
					return nil
				},
				reset: func(_ context.Context, p, f string) error {
					calls = append(calls, "reset")
					if p != j.Principal || f != j.Fingerprint {
						t.Fatal("unguarded reset")
					}
					if failure == "reset" {
						return boom
					}
					return nil
				},
				save: func(s v2UnenrollJournal) error {
					if failure == "save-after-revoke" && s.Revoked {
						return boom
					}
					if failure == "save-after-delete" && s.Deleted {
						return boom
					}
					return nil
				},
			}
			err := performV2Unenroll(context.Background(), &j, ops)
			if (err != nil) != (failure != "") {
				t.Fatalf("err=%v", err)
			}
			expected := map[string][]string{
				"":             {"read", "revoke", "read", "delete", "reset"},
				"initial-read": {"read"}, "revocation": {"read", "revoke"}, "binding-recheck": {"read", "revoke", "read"},
				"delete": {"read", "revoke", "read", "delete"}, "save-after-revoke": {"read", "revoke"}, "save-after-delete": {"read", "revoke", "read", "delete"}, "reset": {"read", "revoke", "read", "delete", "reset"},
			}[failure]
			if !reflect.DeepEqual(calls, expected) {
				t.Fatalf("calls %v want %v", calls, expected)
			}
			if failure == "" && (!j.Reset || !j.Deleted || !j.Revoked) {
				t.Fatal("incomplete result")
			}
		})
	}
}

func TestV2UnenrollBindingRejectsUUIDConfusion(t *testing.T) {
	for _, change := range []func(*cloudpbv2.Asset){
		func(a *cloudpbv2.Asset) { a.Id = unenrollDevice },
		func(a *cloudpbv2.Asset) { a.PkiDeviceName = proto.String(unenrollAsset) },
		func(a *cloudpbv2.Asset) { a.OrganizationId = unenrollAsset },
		func(a *cloudpbv2.Asset) { a.PkiDeviceName = nil },
	} {
		a := testUnenrollAsset()
		change(a)
		if checkV2UnenrollBinding(a, testUnenrollJournal()) == nil {
			t.Fatal("unsafe binding accepted")
		}
	}
}

func TestV2UnenrollResumeAfterLostDeleteResponse(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-proof", true: "revoked-proof"}[acknowledged], func(t *testing.T) {
			j := testUnenrollJournal()
			j.Revoked = acknowledged
			reset := false
			revoked := false
			ops := v2UnenrollOps{
				get: func(context.Context, string) (*cloudpbv2.Asset, error) {
					return nil, status.Error(codes.NotFound, "gone")
				},
				revoke: func(_ context.Context, p, f string) (*agentpbv2.RevokeACMECertificateResponse, error) {
					revoked = true
					return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: p, CertificateSha256: f, CertificateSerial: "42"}, nil
				},
				delete: func(context.Context, string) error { t.Fatal("must not delete another asset"); return nil },
				reset:  func(context.Context, string, string) error { reset = true; return nil }, save: func(v2UnenrollJournal) error { return nil },
			}
			err := performV2Unenroll(context.Background(), &j, ops)
			if acknowledged {
				if err != nil || !reset || !revoked {
					t.Fatalf("resume: %v", err)
				}
			} else if err == nil || reset || revoked {
				t.Fatal("missing row treated as ownership/revocation proof")
			}
		})
	}
}

func TestV2UnenrollRejectsDifferentCertificateAck(t *testing.T) {
	j := testUnenrollJournal()
	destructive := false
	ops := v2UnenrollOps{
		get: func(context.Context, string) (*cloudpbv2.Asset, error) { return testUnenrollAsset(), nil },
		revoke: func(_ context.Context, p, f string) (*agentpbv2.RevokeACMECertificateResponse, error) {
			return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: p, CertificateSha256: strings.Repeat("cd", 32), CertificateSerial: "42"}, nil
		},
		delete: func(context.Context, string) error { destructive = true; return nil }, reset: func(context.Context, string, string) error { destructive = true; return nil }, save: func(v2UnenrollJournal) error { return nil },
	}
	if performV2Unenroll(context.Background(), &j, ops) == nil || destructive {
		t.Fatal("accepted mismatched certificate acknowledgement")
	}
}

func TestV2UnenrollJournalPrivateDurableReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unenroll-v2", "journal.json")
	j := testUnenrollJournal()
	if err := saveV2UnenrollJournal(path, j); err != nil {
		t.Fatal(err)
	}
	j.Revoked = true
	j.Serial = "42"
	if err := saveV2UnenrollJournal(path, j); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"certificateRevoked":true`) {
		t.Fatalf("replacement: %v", err)
	}
}
