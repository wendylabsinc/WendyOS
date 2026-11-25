package appv1

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func testPhysicalClockReply() *PhysicalClockReply {
	c := testClockReply()
	return &PhysicalClockReply{Nonce: c.Nonce, Physical: c.Physical, Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration, ServerBootId: c.ServerBootId}
}

func TestPhysicalClockCodecHasNoSourceSample(t *testing.T) {
	for _, domain := range []string{"physical", "simulation"} {
		reply := testPhysicalClockReply()
		reply.ExecutionDomain = domain
		w, err := DecodePhysicalClockReply(reply)
		if err != nil || !w.Authenticated || w.Domain != "physical" || w.Earliest.UnixNano() != reply.Physical.EarliestUnixNs {
			t.Fatal("physical time required a source sample or changed authority domain", err)
		}
		fields := reply.ProtoReflect().Descriptor().Fields()
		if fields.ByNumber(3) != nil || fields.ByNumber(4) != nil || fields.ByName("sample_unix_ns") != nil || fields.ByName("sample_clock_domain") != nil {
			t.Fatal("physical-only reply contains a source clock")
		}
	}
}

func TestPhysicalClockCodecRefusesMalformedAndUnknownTime(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*PhysicalClockReply){
		"nonce":              func(p *PhysicalClockReply) { p.Nonce = p.Nonce[:31] },
		"scope":              func(p *PhysicalClockReply) { p.DeploymentGeneration = 0 },
		"tenant":             func(p *PhysicalClockReply) { p.Tenant = "other" },
		"physical":           func(p *PhysicalClockReply) { p.Physical = nil },
		"bounds":             func(p *PhysicalClockReply) { p.Physical.LatestUnixNs = p.Physical.EarliestUnixNs - 1 },
		"zero":               func(p *PhysicalClockReply) { p.Physical.EarliestUnixNs = 0 },
		"negative-monotonic": func(p *PhysicalClockReply) { p.Physical.MonotonicNs = -1 },
		"synthetic":          func(p *PhysicalClockReply) { p.Physical.Source = "simulation" },
		"incarnation":        func(p *PhysicalClockReply) { p.ServerBootId = strings.Repeat("A", 32) },
		"unknown":            func(p *PhysicalClockReply) { p.ProtoReflect().SetUnknown(unknown) },
		"nested-unknown":     func(p *PhysicalClockReply) { p.Physical.ProtoReflect().SetUnknown(unknown) },
		"sample-field": func(p *PhysicalClockReply) {
			p.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := proto.Clone(testPhysicalClockReply()).(*PhysicalClockReply)
			mutate(p)
			if _, err := DecodePhysicalClockReply(p); err == nil {
				t.Fatal("malformed physical authority accepted")
			}
		})
	}
	if _, err := DecodePhysicalClockReply(nil); err == nil {
		t.Fatal("nil physical reply accepted")
	}
}
