package interceptor

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type acceptanceStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s acceptanceStream) Context() context.Context { return s.ctx }

// Frozen from the approved 207-row METHOD-ROLE-MATRIX, 2026-11-26.
// Expectations are independent of methodRoles/keylessMethods.
//
//go:embed testdata/auth-modes-matrix.json
var authModesMatrixFS embed.FS

func TestAuthModesA11FrozenMatrix(t *testing.T) {
	type row struct {
		Rank    int
		Off, On bool
	}
	rows := map[string]row{}
	raw, err := authModesMatrixFS.ReadFile("testdata/auth-modes-matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	type method struct{ clientStream, serverStream bool }
	methods := map[string]method{}
	protoregistry.GlobalFiles.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(f.Package()), "wendy.agent.services.") {
			for i := 0; i < f.Services().Len(); i++ {
				service := f.Services().Get(i)
				for j := 0; j < service.Methods().Len(); j++ {
					m := service.Methods().Get(j)
					path := fmt.Sprintf("/%s/%s", service.FullName(), m.Name())
					methods[path] = method{m.IsStreamingClient(), m.IsStreamingServer()}
					if _, ok := rows[path]; !ok {
						t.Errorf("missing contract row %s", path)
					}
				}
			}
		}
		return true
	})
	if len(methods) != 207 || len(rows) != len(methods) {
		t.Fatalf("descriptor methods %d; contract rows %d", len(methods), len(rows))
	}
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	ownerKey, opKey, viewKey := spkiOf(t), spkiOf(t), spkiOf(t)
	actor, err := keys.Close(ownerKey, "fixture owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(opKey, trustedkeys.RoleOperator, "operator", actor.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(viewKey, trustedkeys.RoleViewer, "viewer", actor.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	for _, p := range []struct {
		name     string
		listener ClosedListener
		ctx      context.Context
		rank     int
		keyless  bool
	}{
		{"viewer", ClosedKeyListener, keyCtx(viewKey), 1, false}, {"operator", ClosedKeyListener, keyCtx(opKey), 2, false},
		{"owner", ClosedKeyListener, keyCtx(ownerKey), 3, false}, {"admin", ClosedAdminSocket, bg, 2, false},
		{"keyless-off", ClosedKeylessListener, bg, 0, false}, {"keyless-on", ClosedKeylessListener, bg, 0, true},
	} {
		if p.keyless {
			if err := keys.SetKeylessApps(true, actor.Fingerprint()); err != nil {
				t.Fatal(err)
			}
		}
		auth := ClosedAuthorizer{Logger: zap.NewNop(), Listener: p.listener, Keys: keys}
		admits := 0
		check := func(path string, m method, want bool) {
			entered := 0
			var err error
			if m.clientStream || m.serverStream {
				err = auth.Stream()(nil, acceptanceStream{ctx: p.ctx}, &grpc.StreamServerInfo{FullMethod: path, IsClientStream: m.clientStream, IsServerStream: m.serverStream}, func(any, grpc.ServerStream) error { entered++; return nil })
			} else {
				_, err = auth.Unary()(p.ctx, nil, &grpc.UnaryServerInfo{FullMethod: path}, func(context.Context, any) (any, error) { entered++; return nil, nil })
			}
			t.Logf("%s %s clientStream=%t serverStream=%t expectedAllow=%t entries=%d code=%s", p.name, path, m.clientStream, m.serverStream, want, entered, status.Code(err))
			if want {
				if entered != 1 || err != nil {
					t.Errorf("%s %s: want one handler entry; got %d, %v", p.name, path, entered, err)
				}
			} else if entered != 0 || status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s %s: want denied before handler; got %d, %v", p.name, path, entered, err)
			}
		}
		for path, r := range rows {
			m, ok := methods[path]
			if !ok {
				t.Fatalf("orphan row: %s", path)
			}
			want := p.rank >= r.Rank
			if p.listener == ClosedKeylessListener {
				want = r.Off
				if p.keyless {
					want = r.On
				}
			}
			check(path, m, want)
			if want {
				admits++
			}
		}
		t.Logf("%s: %d expected admitted, %d expected denied, descriptor-selected interceptor checks", p.name, admits, len(rows)-admits)
	}
}
