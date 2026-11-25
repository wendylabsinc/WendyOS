package meshview

import (
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestAnnouncementRouteDeadlineKeepsExactIndependentBinding(t *testing.T) {
	now := time.Now()
	hint := &pb.LocalMeshAnnouncement{Principal: "fixture-principal", Prefix: "fd00::1/128", OriginRouterId: 42, SequenceNumber: 8, TrustState: "verified-at-snapshot", ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}
	old := &pb.LocalMeshRoute{OriginPrincipal: hint.Principal, Prefix: hint.Prefix, OriginRouterId: hint.OriginRouterId, SequenceNumber: 7, Metric: 4096, OriginAuthorized: true, SignedRouteExpiresAtUnixMs: now.Add(5 * time.Second).UnixMilli()}
	for _, tc := range []struct {
		name   string
		mutate func(*pb.LocalMeshRoute, *pb.LocalMeshAnnouncement)
		want   bool
	}{
		{"retained-original-cap", func(*pb.LocalMeshRoute, *pb.LocalMeshAnnouncement) {}, true},
		{"current-exact", func(r *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) {
			r.SequenceNumber = h.SequenceNumber
			r.SignedRouteExpiresAtUnixMs = h.ExpiresAtUnixMs
		}, true},
		{"wrap", func(r *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) {
			r.SequenceNumber = 65535
			h.SequenceNumber = 0
		}, true},
		{"expired-old", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) {
			r.SignedRouteExpiresAtUnixMs = now.UnixMilli()
		}, false},
		{"expired-current", func(_ *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) { h.ExpiresAtUnixMs = now.UnixMilli() }, false},
		{"current-unequal-cap", func(r *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) { r.SequenceNumber = h.SequenceNumber }, false},
		{"old-cap-exceeds-current", func(r *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) {
			r.SignedRouteExpiresAtUnixMs = h.ExpiresAtUnixMs + 1
		}, false},
		{"foreign-principal", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.OriginPrincipal = "foreign" }, false},
		{"foreign-prefix", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.Prefix = "fd00::2/128" }, false},
		{"foreign-router", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.OriginRouterId++ }, false},
		{"unverified-route", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.OriginAuthorized = false }, false},
		{"unverified-current", func(_ *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) { h.TrustState = "" }, false},
		{"unreachable", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.Unreachable = true }, false},
		{"infinity", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.Metric = 65535 }, false},
		{"newer-route", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.SequenceNumber = 9 }, false},
		{"ambiguous-half-range", func(r *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) {
			r.SequenceNumber = uint32(uint16(h.SequenceNumber) + 32768)
		}, false},
		{"invalid-route-sequence", func(r *pb.LocalMeshRoute, _ *pb.LocalMeshAnnouncement) { r.SequenceNumber = 65536 }, false},
		{"invalid-current-sequence", func(_ *pb.LocalMeshRoute, h *pb.LocalMeshAnnouncement) { h.SequenceNumber = 65536 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proto.Clone(old).(*pb.LocalMeshRoute)
			h := proto.Clone(hint).(*pb.LocalMeshAnnouncement)
			tc.mutate(r, h)
			deadline, ok := AnnouncementRouteDeadline(r, h, now)
			if ok != tc.want || ok && deadline != r.SignedRouteExpiresAtUnixMs {
				t.Fatalf("deadline=%d accepted=%v want=%v", deadline, ok, tc.want)
			}
		})
	}
}
