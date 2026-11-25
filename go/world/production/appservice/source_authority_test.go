package appservice

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func brokerSourceRequest() *appv1.GetSourceAuthorityRequest {
	return &appv1.GetSourceAuthorityRequest{Nonce: bytes.Repeat([]byte{1}, appv1.ClockNonceSize), Sources: []*appv1.SourceSelector{{App: "com.example.camera", Service: "camera", ComponentType: "wendy.capture.v1", Stream: "north"}, {App: "com.example.gas", Service: "gas", ComponentType: "wendy.capture.v1", Stream: "gas"}}}
}

func brokerSourceReply(scope serviceorigin.Scope, nonce []byte, sources []serviceaccess.Source) *appv1.SourceAuthorityReply {
	p := brokerPhysicalReply(scope, nonce)
	r := &appv1.SourceAuthorityReply{Nonce: p.Nonce, Physical: p.Physical, Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: p.ExecutionDomain, DeploymentGeneration: p.DeploymentGeneration, ServerBootId: p.ServerBootId}
	for _, s := range sources {
		r.Authorities = append(r.Authorities, &appv1.SourceAuthority{Source: &appv1.SourceSelector{Device: s.Device, App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream}, SignedDelegationJson: []byte(`{"shape_only":true}`)})
	}
	return r
}

func TestSourceAuthorityBrokerRequiresFenceAndPreservesWholeSelection(t *testing.T) {
	var fenced atomic.Bool
	f := setup(t, func(cfg *Config) {
		cfg.SourceAuthority = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, sources []serviceaccess.Source, fence *Fence) (*appv1.SourceAuthorityReply, error) {
			r := brokerSourceReply(scope, nonce, sources)
			if !fenced.Load() {
				return r, nil
			}
			return r, fence.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := brokerSourceRequest()
	req.Sources[0].Device = f.scope.Device
	if out, err := client.GetSourceAuthority(ctx, req); status.Code(err) != codes.Unavailable || out != nil {
		t.Fatal("unfenced metadata accepted", err)
	}
	fenced.Store(true)
	nonce, sources, err := appv1.DecodeSourceAuthorityRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.GetSourceAuthority(ctx, req)
	if err != nil || !proto.Equal(out, brokerSourceReply(f.scope, nonce, sources)) || f.count() != 0 {
		t.Fatal("source selection changed or metadata published", err)
	}
}

func TestSourceAuthorityBrokerRefusesPartialReorderedAndWrongReaderReplies(t *testing.T) {
	for name, mutate := range map[string]func(*appv1.SourceAuthorityReply){
		"nonce":      func(p *appv1.SourceAuthorityReply) { p.Nonce[0]++ },
		"reader":     func(p *appv1.SourceAuthorityReply) { p.Service = "other" },
		"generation": func(p *appv1.SourceAuthorityReply) { p.DeploymentGeneration++ },
		"partial":    func(p *appv1.SourceAuthorityReply) { p.Authorities = p.Authorities[:1] },
		"reordered": func(p *appv1.SourceAuthorityReply) {
			p.Authorities[0], p.Authorities[1] = p.Authorities[1], p.Authorities[0]
		},
		"normalized-request": func(p *appv1.SourceAuthorityReply) { p.Authorities[0].Source.Device = p.Device },
		"different-stream":   func(p *appv1.SourceAuthorityReply) { p.Authorities[0].Source.Stream = "south" },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, func(cfg *Config) {
				cfg.SourceAuthority = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, sources []serviceaccess.Source, fence *Fence) (*appv1.SourceAuthorityReply, error) {
					r := brokerSourceReply(scope, nonce, sources)
					mutate(r)
					return r, fence.Commit(ctx, func(context.Context) error { return nil })
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if out, err := client.GetSourceAuthority(ctx, brokerSourceRequest()); status.Code(err) != codes.Unavailable || out != nil {
				t.Fatal("unbound response accepted", err)
			}
		})
	}
}

func TestSourceAuthorityBrokerRefusesUnavailableAuthorityAndMalformedRequest(t *testing.T) {
	for _, mode := range []string{"missing", "denied", "finalizer"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, func(cfg *Config) {
				if mode == "missing" {
					return
				}
				cfg.SourceAuthority = func(ctx context.Context, scope serviceorigin.Scope, nonce []byte, sources []serviceaccess.Source, fence *Fence) (*appv1.SourceAuthorityReply, error) {
					if mode == "denied" {
						return nil, ErrAdmission
					}
					return brokerSourceReply(scope, nonce, sources), fence.Commit(ctx, func(context.Context) error { return nil })
				}
				if mode == "finalizer" {
					cfg.FinalizeResponse = func(context.Context, serviceorigin.Scope, proto.Message) (proto.Message, error) {
						return nil, ErrAdmission
					}
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			want := codes.Unimplemented
			if mode == "denied" {
				want = codes.PermissionDenied
			}
			if mode == "finalizer" {
				want = codes.Unavailable
			}
			if out, err := client.GetSourceAuthority(ctx, brokerSourceRequest()); status.Code(err) != want || out != nil {
				t.Fatal("authority failure returned metadata", err)
			}
			if out, err := client.GetSourceAuthority(ctx, &appv1.GetSourceAuthorityRequest{}); status.Code(err) != codes.InvalidArgument || out != nil {
				t.Fatal("empty request accepted", err)
			}
		})
	}
}
