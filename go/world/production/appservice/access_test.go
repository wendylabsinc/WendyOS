package appservice

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func brokerAccessSource() serviceaccess.Source {
	return serviceaccess.Source{App: "com.example.camera", Service: "detector-a", ComponentType: "wendy.pose.v1", Stream: "people"}
}
func brokerAccessQuery() *appv1.QueryRequest {
	s := brokerAccessSource()
	return &appv1.QueryRequest{Sources: []*appv1.SourceSelector{{App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream}}, Limit: 1}
}

func TestAccessReadOnlyPublisherCannotSignAndRequiresFencedReads(t *testing.T) {
	var calls atomic.Int32
	var fenced atomic.Bool
	f := setup(t, func(cfg *Config) {
		cfg.Query = func(ctx context.Context, scope serviceorigin.Scope, q serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
			calls.Add(1)
			if len(scope.Publish) != 0 || q.Limit != 1 || len(q.Sources) != 1 || q.Sources[0] != brokerAccessSource() {
				return serviceaccess.Page{}, ErrAdmission
			}
			page := serviceaccess.Page{SignedRecordsJSON: [][]byte{[]byte("original-proof-bytes")}}
			if !fenced.Load() {
				return page, nil
			}
			return page, fence.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	scope := f.scope
	scope.Publish = nil
	mount := f.mount(t, scope)
	client, _ := dial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := client.Publish(ctx, request()); status.Code(err) != codes.PermissionDenied || out != nil || f.count() != 0 {
		t.Fatal("empty publication scope signed a record", err)
	}
	stream, err := client.PublishStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(request()); err != nil {
		t.Fatal(err)
	}
	if out, err := stream.Recv(); status.Code(err) != codes.PermissionDenied || out != nil || f.count() != 0 {
		t.Fatal("empty publication scope signed a streamed record", err)
	}
	if out, err := client.Query(ctx, brokerAccessQuery()); status.Code(err) != codes.Unavailable || out != nil {
		t.Fatal("unfenced callback exposed read bytes", err)
	}
	fenced.Store(true)
	if out, err := client.Query(ctx, brokerAccessQuery()); err != nil || len(out.GetSignedRecordJson()) != 1 || string(out.SignedRecordJson[0]) != "original-proof-bytes" {
		t.Fatal("fenced callback changed original bytes", err)
	}
	bad := brokerAccessQuery()
	bad.Sources[0].ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 20, protowire.BytesType), "forged-owner"))
	if out, err := client.Query(ctx, bad); status.Code(err) != codes.InvalidArgument || out != nil || calls.Load() != 2 {
		t.Fatal("nested unknown identity reached query callback", err)
	}
}

func TestAccessResponseBudgetAndFinalAuthorityCheckExposeNoBytes(t *testing.T) {
	for _, mode := range []string{"oversize", "authority-change"} {
		t.Run(mode, func(t *testing.T) {
			var revoke func()
			f := setup(t, func(cfg *Config) {
				cfg.Limits.MaxResponseBytes = 1024
				cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
					if err := fence.Commit(ctx, func(context.Context) error { return nil }); err != nil {
						return serviceaccess.Page{}, err
					}
					if mode == "authority-change" {
						revoke()
						return serviceaccess.Page{SignedRecordsJSON: [][]byte{[]byte("must stay private")}}, nil
					}
					return serviceaccess.Page{SignedRecordsJSON: [][]byte{bytes.Repeat([]byte("x"), 2048)}}, nil
				}
			})
			mount := f.mount(t, f.scope)
			revoke = func() { f.publishers[f.scope.Service].Close() }
			client, _ := dial(t, mount.SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if out, err := client.Query(ctx, brokerAccessQuery()); err == nil || out != nil {
				t.Fatal("unbounded or revoked response bytes escaped")
			}
		})
	}
}

// A real child process connects to each access method. No client-supplied
// source selector may replace the kernel peer and commissioned deployment.
func TestAccessLinuxExactPeerAcrossQueryAndEvidence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real Linux peer credentials and pidfd required")
	}
	type commissioned struct {
		service    string
		generation uint64
	}
	var mu sync.Mutex
	peers := map[int32]commissioned{}
	var accepted atomic.Int32
	f := setup(t, func(cfg *Config) {
		cfg.PeerCheck = func(_ context.Context, p Peer, s serviceorigin.Scope) error {
			mu.Lock()
			defer mu.Unlock()
			if b, ok := peers[p.PID]; !ok || b.service != s.Service || b.generation != s.DeploymentGeneration {
				return ErrPeer
			}
			return nil
		}
		commit := func(ctx context.Context, fence *Fence) error {
			return fence.Commit(ctx, func(context.Context) error { accepted.Add(1); return nil })
		}
		cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, f *Fence) (serviceaccess.Page, error) {
			return serviceaccess.Page{}, commit(ctx, f)
		}
		cfg.PutEvidence = func(ctx context.Context, _ serviceorigin.Scope, _ serviceorigin.PublishScope, _ serviceevidence.PutRequest, f *Fence) (serviceevidence.PutResponse, error) {
			return serviceevidence.PutResponse{Complete: true}, commit(ctx, f)
		}
		cfg.GetEvidence = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Source, _ serviceevidence.GetRequest, f *Fence) (serviceevidence.GetResponse, error) {
			return serviceevidence.GetResponse{Data: []byte("retained")}, commit(ctx, f)
		}
	})
	mount := f.mount(t, f.scope)
	run := func(service string, generation uint64) string {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAccessPeerProcess$")
		cmd.Env = append(os.Environ(), "WENDY_WORLD_ACCESS_PEER="+mount.SocketPath)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		peers[int32(cmd.Process.Pid)] = commissioned{service, generation}
		mu.Unlock()
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("access child failed: %v: %s", err, output.String())
		}
		mu.Lock()
		delete(peers, int32(cmd.Process.Pid))
		mu.Unlock()
		return strings.TrimSpace(output.String())
	}
	if got := run(f.scope.Service, f.scope.DeploymentGeneration); got != "true true true" {
		t.Fatal("commissioned peer denied", got)
	}
	if got := run("detector-b", f.scope.DeploymentGeneration); got != "false false false" {
		t.Fatal("foreign service read or wrote", got)
	}
	if got := run(f.scope.Service, f.scope.DeploymentGeneration+1); got != "false false false" {
		t.Fatal("foreign generation read or wrote", got)
	}
	if accepted.Load() != 3 {
		t.Fatal("denied peer reached access callback", accepted.Load())
	}
}

func TestAccessPeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_WORLD_ACCESS_PEER")
	if path == "" {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	client, _ := dial(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, queryErr := client.Query(ctx, brokerAccessQuery())
	_, putErr := client.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: "wendy.pose.v1", Stream: "people", Sha256: strings.Repeat("a", 64), Size: 1, Data: []byte("x"), ContentType: "image/jpeg"})
	_, getErr := client.GetEvidence(ctx, &appv1.GetEvidenceRequest{Source: brokerAccessQuery().Sources[0], Sha256: strings.Repeat("a", 64), Length: 1})
	for i, err := range []error{queryErr, putErr, getErr} {
		if i > 0 {
			_, _ = os.Stdout.WriteString(" ")
		}
		if err == nil {
			_, _ = os.Stdout.WriteString("true")
		} else {
			_, _ = os.Stdout.WriteString("false")
		}
	}
	_, _ = os.Stdout.WriteString("\n")
	os.Exit(0)
}
