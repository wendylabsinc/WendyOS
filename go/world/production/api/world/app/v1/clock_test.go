package appv1

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func testClockReply() *ClockReply {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC).UnixNano()
	tenant := "11111111-1111-4111-8111-111111111111"
	return &ClockReply{Nonce: bytes.Repeat([]byte{1}, ClockNonceSize), Physical: &PhysicalClock{EarliestUnixNs: now, LatestUnixNs: now + int64(time.Millisecond), BootId: "host-boot", MonotonicNs: int64(time.Second), Source: "roughtime-verified"}, SampleClockDomain: "simulation:office", SampleUnixNs: now, Tenant: tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222", App: "com.example.fusion", Service: "fusion", ExecutionDomain: "simulation", DeploymentGeneration: 7, ServerBootId: strings.Repeat("a", 32)}
}

func TestClockCodecSeparatesSourceTimeAndPhysicalAuthority(t *testing.T) {
	reply := testClockReply()
	reply.SampleUnixNs -= int64(24 * time.Hour)
	w, err := DecodeClockReply(reply)
	if err != nil || w.Earliest.UnixNano() != reply.Physical.EarliestUnixNs || w.Domain != "physical" || !w.Authenticated || w.BootID != reply.Physical.BootId {
		t.Fatal("valid independent clock domains rejected", err)
	}
	request := &ClockRequest{Nonce: bytes.Clone(reply.Nonce)}
	nonce, err := DecodeClock(request)
	if err != nil || !bytes.Equal(nonce, request.Nonce) {
		t.Fatal(err)
	}
	nonce[0]++
	if bytes.Equal(nonce, request.Nonce) {
		t.Fatal("request aliases returned nonce")
	}
}

func TestClockCodecRejectsMalformedAndUnknownAuthority(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*ClockReply){
		"nonce":               func(p *ClockReply) { p.Nonce = p.Nonce[:31] },
		"scope":               func(p *ClockReply) { p.DeploymentGeneration = 0 },
		"tenant":              func(p *ClockReply) { p.Tenant = "other-tenant" },
		"physical-clock":      func(p *ClockReply) { p.Physical = nil },
		"bounds":              func(p *ClockReply) { p.Physical.LatestUnixNs = p.Physical.EarliestUnixNs - 1 },
		"negative-monotonic":  func(p *ClockReply) { p.Physical.MonotonicNs = -1 },
		"sample":              func(p *ClockReply) { p.SampleUnixNs = 0 },
		"source-clock-domain": func(p *ClockReply) { p.SampleClockDomain = "utc" },
		"physical-domain":     func(p *ClockReply) { p.ExecutionDomain = "physical" },
		"synthetic-authority": func(p *ClockReply) { p.Physical.Source = "simulation" },
		"incarnation":         func(p *ClockReply) { p.ServerBootId = strings.Repeat("A", 32) },
		"unknown":             func(p *ClockReply) { p.ProtoReflect().SetUnknown(unknown) },
		"nested-unknown":      func(p *ClockReply) { p.Physical.ProtoReflect().SetUnknown(unknown) },
	} {
		t.Run(name, func(t *testing.T) {
			p := proto.Clone(testClockReply()).(*ClockReply)
			mutate(p)
			if _, err := DecodeClockReply(p); err == nil {
				t.Fatal("invalid clock authority accepted")
			}
		})
	}
	for _, request := range []*ClockRequest{nil, {}, {Nonce: make([]byte, 33)}, {Nonce: make([]byte, 32)}} {
		if request != nil && len(request.Nonce) == 32 {
			request.ProtoReflect().SetUnknown(unknown)
		}
		if _, err := DecodeClock(request); err == nil {
			t.Fatal("invalid clock request accepted")
		}
	}
}
