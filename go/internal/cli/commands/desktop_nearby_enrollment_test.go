package commands

import (
	"context"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

func TestNearbyEnrollmentPresenceRequiresIdentityAndFreshHeartbeat(t *testing.T) {
	c := &DesktopNearbyCredential{DeviceID: "device-id", Organization: "org-id", Lite: true, RequestedAt: time.Now()}
	valid := &cloudpb.Asset{Id: c.DeviceID, OrganizationId: c.Organization, OsType: proto.String("wendy-lite"), LastHeartbeat: timestamppb.Now()}
	if !nearbyEnrollmentPresent(valid, c) {
		t.Fatal("valid live device not recognized")
	}
	for name, change := range map[string]func(*cloudpb.Asset){
		"inventory only":   func(d *cloudpb.Asset) { d.LastHeartbeat = nil },
		"wrong identity":   func(d *cloudpb.Asset) { d.Id = "other" },
		"wrong tenant":     func(d *cloudpb.Asset) { d.OrganizationId = "other" },
		"wrong OS":         func(d *cloudpb.Asset) { d.OsType = proto.String("linux") },
		"old heartbeat":    func(d *cloudpb.Asset) { d.LastHeartbeat = timestamppb.New(c.RequestedAt.Add(-time.Minute)) },
		"future heartbeat": func(d *cloudpb.Asset) { d.LastHeartbeat = timestamppb.New(time.Now().Add(time.Hour)) },
		"conflicting PKI":  func(d *cloudpb.Asset) { d.PkiDeviceName = proto.String("other") },
	} {
		t.Run(name, func(t *testing.T) {
			d := proto.Clone(valid).(*cloudpb.Asset)
			change(d)
			if nearbyEnrollmentPresent(d, c) {
				t.Fatal("accepted unrelated or stale cloud record")
			}
		})
	}
}
func TestNearbyEnrollmentDoesNotAcceptOldPresenceOnLaterCheck(t *testing.T) {
	c := &DesktopNearbyCredential{DeviceID: "device", Organization: "org", RequestedAt: time.Now().Add(-time.Hour)}
	stale := &cloudpb.Asset{Id: c.DeviceID, OrganizationId: c.Organization, LastHeartbeat: timestamppb.New(time.Now().Add(-10 * time.Minute))}
	if nearbyEnrollmentPresent(stale, c) {
		t.Fatal("accepted a device that was online earlier but is offline now")
	}
}
func TestNearbyPreparationRejectsInvalidInputBeforeMint(t *testing.T) {
	for _, name := range []string{"", "UPPER", "-leading", "trailing-", "1leading", "spaces here", strings.Repeat("x", 64)} {
		if NearbyEnrollmentNameValid(name) {
			t.Errorf("accepted invalid name %q", name)
		}
		_, err := DesktopPrepareNearbyEnrollment(context.Background(), "account", "org", name, &pb.SetupStatusResponse{DeviceClass: "C", LiteSupported: true}, func() { t.Fatal("minted invalid input") })
		if err == nil {
			t.Error("accepted invalid preparation")
		}
	}
	for _, name := range []string{"a", "a-b2", strings.Repeat("x", 63)} {
		if !NearbyEnrollmentNameValid(name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, state := range []*pb.SetupStatusResponse{nil, {}, {Enrolled: true, AcmeSupported: true}} {
		if _, err := DesktopPrepareNearbyEnrollment(context.Background(), "account", "org", "valid", state, func() { t.Fatal("minted unavailable device") }); err == nil {
			t.Fatal("accepted unavailable device")
		}
	}
}
