package appv1

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func testSourceAuthorityRequest() *GetSourceAuthorityRequest {
	return &GetSourceAuthorityRequest{Nonce: bytes.Repeat([]byte{1}, ClockNonceSize), Sources: []*SourceSelector{{App: "com.example.camera", Service: "camera", ComponentType: "wendy.capture.v1", Stream: "north"}}}
}

func testSourceAuthorityReply() *SourceAuthorityReply {
	p := testPhysicalClockReply()
	return &SourceAuthorityReply{Nonce: p.Nonce, Physical: p.Physical, Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: p.ExecutionDomain, DeploymentGeneration: p.DeploymentGeneration, ServerBootId: p.ServerBootId, Authorities: []*SourceAuthority{{Source: testSourceAuthorityRequest().Sources[0], SignedDelegationJson: []byte(`{"shape_only":true}`)}}}
}

func TestSourceAuthorityRequestPreservesExplicitSelectionAndOwnsNonce(t *testing.T) {
	p := testSourceAuthorityRequest()
	p.Sources[0].Device = testClockReply().Device
	nonce, sources, err := DecodeSourceAuthorityRequest(p)
	if err != nil || len(sources) != 1 || sources[0].Device != p.Sources[0].Device || sources[0].Stream != "north" {
		t.Fatal("qualified selection changed", err)
	}
	nonce[0]++
	if bytes.Equal(nonce, p.Nonce) {
		t.Fatal("nonce aliases request")
	}
	p.Sources[0].Device = ""
	_, sources, err = DecodeSourceAuthorityRequest(p)
	if err != nil || sources[0].Device != "" {
		t.Fatal("implicit selection changed", err)
	}
}

func TestSourceAuthorityRequestRejectsMalformedOrUnboundedSelection(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*GetSourceAuthorityRequest){
		"nonce":      func(p *GetSourceAuthorityRequest) { p.Nonce = p.Nonce[:31] },
		"empty":      func(p *GetSourceAuthorityRequest) { p.Sources = nil },
		"nil-source": func(p *GetSourceAuthorityRequest) { p.Sources[0] = nil },
		"wildcard":   func(p *GetSourceAuthorityRequest) { p.Sources[0].Stream = "*" },
		"duplicate": func(p *GetSourceAuthorityRequest) {
			p.Sources = append(p.Sources, proto.Clone(p.Sources[0]).(*SourceSelector))
		},
		"over-limit": func(p *GetSourceAuthorityRequest) {
			for len(p.Sources) <= MaxAuthoritySources {
				p.Sources = append(p.Sources, p.Sources[0])
			}
		},
		"unknown":        func(p *GetSourceAuthorityRequest) { p.ProtoReflect().SetUnknown(unknown) },
		"nested-unknown": func(p *GetSourceAuthorityRequest) { p.Sources[0].ProtoReflect().SetUnknown(unknown) },
	} {
		t.Run(name, func(t *testing.T) {
			p := testSourceAuthorityRequest()
			mutate(p)
			if _, _, err := DecodeSourceAuthorityRequest(p); err == nil {
				t.Fatal("invalid source request accepted")
			}
		})
	}
	if _, _, err := DecodeSourceAuthorityRequest(nil); err == nil {
		t.Fatal("nil source request accepted")
	}
}

func TestSourceAuthorityReplyIsLocalBoundedShapeNotProofVerification(t *testing.T) {
	p := testSourceAuthorityReply()
	w, err := DecodeSourceAuthorityReply(p)
	if err != nil || !w.Authenticated || w.Domain != "physical" {
		t.Fatal("valid response shape rejected", err)
	}
	// Delegation authentication belongs to the consumer; the codec only bounds JSON.
	p.Authorities[0].Source.Device = p.Device
	if _, err = DecodeSourceAuthorityReply(p); err != nil {
		t.Fatal("explicit local source rejected", err)
	}
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	for name, mutate := range map[string]func(*SourceAuthorityReply){
		"empty":        func(p *SourceAuthorityReply) { p.Authorities = nil },
		"nil-entry":    func(p *SourceAuthorityReply) { p.Authorities[0] = nil },
		"nil-source":   func(p *SourceAuthorityReply) { p.Authorities[0].Source = nil },
		"empty-proof":  func(p *SourceAuthorityReply) { p.Authorities[0].SignedDelegationJson = nil },
		"invalid-json": func(p *SourceAuthorityReply) { p.Authorities[0].SignedDelegationJson = []byte("{") },
		"oversize": func(p *SourceAuthorityReply) {
			p.Authorities[0].SignedDelegationJson = []byte(`"` + strings.Repeat("x", MaxAuthorityReplyBytes) + `"`)
		},
		"remote": func(p *SourceAuthorityReply) {
			p.Authorities[0].Source.Device = strings.Replace(p.Device, "22222222-", "33333333-", 1)
		},
		"alias-duplicate": func(p *SourceAuthorityReply) {
			q := proto.Clone(p.Authorities[0]).(*SourceAuthority)
			q.Source.Device = p.Device
			p.Authorities = append(p.Authorities, q)
		},
		"nonce":            func(p *SourceAuthorityReply) { p.Nonce = nil },
		"clock":            func(p *SourceAuthorityReply) { p.Physical.LatestUnixNs = 0 },
		"unknown":          func(p *SourceAuthorityReply) { p.ProtoReflect().SetUnknown(unknown) },
		"entry-unknown":    func(p *SourceAuthorityReply) { p.Authorities[0].ProtoReflect().SetUnknown(unknown) },
		"selector-unknown": func(p *SourceAuthorityReply) { p.Authorities[0].Source.ProtoReflect().SetUnknown(unknown) },
		"clock-unknown":    func(p *SourceAuthorityReply) { p.Physical.ProtoReflect().SetUnknown(unknown) },
	} {
		t.Run(name, func(t *testing.T) {
			p := testSourceAuthorityReply()
			mutate(p)
			if _, err := DecodeSourceAuthorityReply(p); err == nil {
				t.Fatal("invalid source reply accepted")
			}
		})
	}
	if _, err := DecodeSourceAuthorityReply(nil); err == nil {
		t.Fatal("nil source reply accepted")
	}
}
