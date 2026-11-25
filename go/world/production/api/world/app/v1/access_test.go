package appv1

import (
	"bytes"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestAccessRefusesUnknownIdentityAndUnboundedReads(t *testing.T) {
	selector := &SourceSelector{App: "com.example.sensors", Service: "camera", ComponentType: "wendy.sensor.capture.v1", Stream: "front"}
	q := &QueryRequest{Sources: []*SourceSelector{selector}, Limit: 8}
	if _, e := DecodeQuery(q); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*QueryRequest){
		"wildcard":  func(p *QueryRequest) { p.Sources[0].Stream = "*" },
		"duplicate": func(p *QueryRequest) { p.Sources = append(p.Sources, p.Sources[0]) },
		"zero":      func(p *QueryRequest) { p.Limit = 0 },
		"large":     func(p *QueryRequest) { p.Limit = 9 },
		"empty":     func(p *QueryRequest) { p.Sources = nil },
		"cursor":    func(p *QueryRequest) { p.After = "forged" },
		"caller identity": func(p *QueryRequest) {
			p.Sources[0].ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 100, protowire.BytesType), "another-device"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := proto.Clone(q).(*QueryRequest)
			change(p)
			if _, e := DecodeQuery(p); e == nil {
				t.Fatal("unsafe query accepted")
			}
		})
	}
}
func TestEvidenceWireBoundsAndOwnedBytes(t *testing.T) {
	p := &PutEvidenceRequest{ComponentType: "wendy.sensor.capture.v1", Stream: "front", Sha256: strings.Repeat("a", 64), Size: serviceevidence.MaxChunk, Data: bytes.Repeat([]byte{42}, serviceevidence.MaxChunk), ContentType: "image/png"}
	decoded, e := DecodePutEvidence(p)
	if e != nil {
		t.Fatal(e)
	}
	p.Data[0] = 0
	if decoded.Data[0] != 42 {
		t.Fatal("upload buffer aliases transport")
	}
	for name, change := range map[string]func(*PutEvidenceRequest){
		"overflow":     func(p *PutEvidenceRequest) { p.Offset = p.Size + 1 },
		"negative":     func(p *PutEvidenceRequest) { p.Offset = -1 },
		"excess":       func(p *PutEvidenceRequest) { p.Data = append(p.Data, 1) },
		"beyond total": func(p *PutEvidenceRequest) { p.Size-- },
		"case digest":  func(p *PutEvidenceRequest) { p.Sha256 = strings.ToUpper(p.Sha256) },
		"content":      func(p *PutEvidenceRequest) { p.ContentType = "text/plain" },
		"owner": func(p *PutEvidenceRequest) {
			p.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 100, protowire.BytesType), "sibling"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := proto.Clone(p).(*PutEvidenceRequest)
			change(v)
			if _, e := DecodePutEvidence(v); e == nil {
				t.Fatal("invalid upload accepted")
			}
		})
	}
	get := &GetEvidenceRequest{Source: &SourceSelector{App: "com.example.sensors", Service: "camera", ComponentType: p.ComponentType, Stream: p.Stream}, Sha256: p.Sha256, Length: serviceevidence.MaxChunk}
	if _, _, e := DecodeGetEvidence(get); e != nil {
		t.Fatal(e)
	}
	get.Length++
	if _, _, e := DecodeGetEvidence(get); e == nil {
		t.Fatal("oversize read accepted")
	}
	get.Length = 1
	get.Source = nil
	if _, _, e := DecodeGetEvidence(get); e == nil {
		t.Fatal("digest without source accepted")
	}
}
