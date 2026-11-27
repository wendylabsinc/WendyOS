package appv1

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestGetRecordProofResolvesOnlyCommissionedDevice(t *testing.T) {
	c := testClockReply()
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration}
	request := &GetRecordProofRequest{Source: &SourceSelector{App: "com.example.camera", Service: "capture", ComponentType: "wendy.sensor.capture.v1", Stream: "front"}, Entity: "capture-9", Epoch: 2, Sequence: 9, EnvelopeSha256: strings.Repeat("a", 64), RecordSha256: strings.Repeat("b", 64)}
	got, err := DecodeGetRecordProof(request, scope)
	if err != nil || got.Key.Tenant != scope.Tenant || got.Key.Site != scope.Site || got.Key.Source.Device != scope.Device || got.Key.Source.App != request.Source.App || got.Key.Source.Service != request.Source.Service || got.Key.Entity != request.Entity || got.Version.Epoch != 2 || got.Version.Sequence != 9 || got.EnvelopeSHA256 != request.EnvelopeSha256 || got.RecordSHA256 != request.RecordSha256 {
		t.Fatal("proof reference lost exact source or version", err)
	}
	for name, change := range map[string]func(*GetRecordProofRequest){
		"unknown-identity": func(p *GetRecordProofRequest) {
			p.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 20, protowire.BytesType), "foreign-device"))
		},
		"unknown-source": func(p *GetRecordProofRequest) {
			p.Source.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 20, protowire.BytesType), "foreign-device"))
		},
		"wildcard":       func(p *GetRecordProofRequest) { p.Source.Stream = "*" },
		"missing-source": func(p *GetRecordProofRequest) { p.Source = nil },
		"epoch":          func(p *GetRecordProofRequest) { p.Epoch = 0 },
		"sequence":       func(p *GetRecordProofRequest) { p.Sequence = 0 },
		"digest":         func(p *GetRecordProofRequest) { p.RecordSha256 = strings.Repeat("A", 64) },
		"entity":         func(p *GetRecordProofRequest) { p.Entity = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := proto.Clone(request).(*GetRecordProofRequest)
			change(bad)
			if _, err := DecodeGetRecordProof(bad, scope); err == nil {
				t.Fatal("invalid or broadened exact reference accepted")
			}
		})
	}
}
