package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type pruneWireServer struct {
	agentpbv2.UnimplementedWendyContainerServiceServer
	requests chan *agentpbv2.PruneCacheRequest
	err      error
}

func (s *pruneWireServer) PruneCache(_ context.Context, req *agentpbv2.PruneCacheRequest) (*agentpbv2.PruneCacheResponse, error) {
	s.requests <- proto.Clone(req).(*agentpbv2.PruneCacheRequest)
	if s.err != nil {
		return nil, s.err
	}
	age := uint64(86400)
	if req.MinAgeSeconds != nil {
		age = req.GetMinAgeSeconds()
	}
	return &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: age, ContentBlobs: 2, ContentBytes: 4096, ReclaimedBytes: proto.Uint64(1024)}, nil
}

func startPruneWireServer(t *testing.T, failure error) *pruneWireServer {
	t.Helper()
	setTempConfig(t, &config.Config{})
	oldDevice, oldJSON := deviceFlag, jsonOutput
	deviceFlag, jsonOutput = "", false
	t.Cleanup(func() { deviceFlag, jsonOutput = oldDevice, oldJSON })
	s := &pruneWireServer{requests: make(chan *agentpbv2.PruneCacheRequest, 8), err: failure}
	startUDSAgent(t, func(server *grpc.Server) { agentpbv2.RegisterWendyContainerServiceServer(server, s) })
	return s
}

// Parse actual Cobra arguments and inspect the protobuf received over the
// local Agent socket. A helper-only test misses flags dropped by RunE.
func TestCachePruneCommandWireOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		want   *agentpbv2.PruneCacheRequest
		output string
	}{
		{"default", nil, &agentpbv2.PruneCacheRequest{}, "Released:"},
		{"all", []string{"--all"}, &agentpbv2.PruneCacheRequest{MinAgeSeconds: proto.Uint64(0)}, "Released:"},
		{"age", []string{"--min-age", "2h3m4s"}, &agentpbv2.PruneCacheRequest{MinAgeSeconds: proto.Uint64(7384)}, "Released:"},
		{"dry default", []string{"--dry-run"}, &agentpbv2.PruneCacheRequest{DryRun: true}, "Eligible:"},
		{"dry all", []string{"--dry-run", "--all"}, &agentpbv2.PruneCacheRequest{DryRun: true, MinAgeSeconds: proto.Uint64(0)}, "Eligible:"},
		{"dry age", []string{"--dry-run", "--min-age=1s"}, &agentpbv2.PruneCacheRequest{DryRun: true, MinAgeSeconds: proto.Uint64(1)}, "Eligible:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startPruneWireServer(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := newDeviceCacheCmd()
			cmd.SetArgs(append([]string{"prune"}, tc.args...))
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-s.requests:
				if !proto.Equal(got, tc.want) {
					t.Fatalf("wire request = %v, want %v", got, tc.want)
				}
			default:
				t.Fatal("command returned without a PruneCache RPC")
			}
			if len(s.requests) != 0 {
				t.Fatal("command issued duplicate prune requests")
			}
			if !strings.Contains(out.String(), tc.output) || errOut.Len() != 0 {
				t.Fatalf("out=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

func TestCachePruneCommandRejectsInvalidOptionsBeforeRPC(t *testing.T) {
	for _, args := range [][]string{
		{"--all", "--min-age=1h"}, {"--min-age=-1s"}, {"--min-age=1ms"}, {"--min-age=tomorrow"}, {"unexpected"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			s := startPruneWireServer(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := newDeviceCacheCmd()
			cmd.SetArgs(append([]string{"prune"}, args...))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.ExecuteContext(ctx); err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if len(s.requests) != 0 {
				t.Fatal("invalid arguments reached PruneCache")
			}
		})
	}
}

// Execute the command returned by the actual p-key handler, then feed its
// real RPC result back into the model. No terminal rendering is needed.
func TestTopPruneKeyReachesAgentAndReportsResult(t *testing.T) {
	for _, failure := range []error{nil, status.Error(codes.PermissionDenied, "cache is protected")} {
		t.Run(status.Code(failure).String(), func(t *testing.T) {
			s := startPruneWireServer(t, failure)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := connectToAgent(ctx, SuppressUpdateCheck())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			m := newTopModel(ctx, conn, time.Second)
			updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
			m = updated.(topModel)
			if cmd == nil || !m.pruningCache {
				t.Fatal("prune key did not initiate action")
			}
			if _, duplicate := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}); duplicate != nil {
				t.Fatal("duplicate prune accepted while busy")
			}
			result, ok := cmd().(topPruneResultMsg)
			if !ok {
				t.Fatal("prune returned wrong result type")
			}
			select {
			case req := <-s.requests:
				if req.MinAgeSeconds != nil || req.GetDryRun() {
					t.Fatalf("top must use Agent grace period and perform prune: %v", req)
				}
			default:
				t.Fatal("p-key command did not reach Agent")
			}
			if len(s.requests) != 0 {
				t.Fatal("p-key issued duplicate RPCs")
			}
			updated, _ = m.Update(result)
			m = updated.(topModel)
			if m.pruningCache {
				t.Fatal("completed action stayed busy")
			}
			if failure == nil {
				if result.err != nil || !strings.HasPrefix(m.actionStatus, "Released:") || strings.Contains(m.actionStatus, "\n") {
					t.Fatalf("success=%q err=%v", m.actionStatus, result.err)
				}
			} else if status.Code(result.err) != codes.PermissionDenied || !strings.Contains(m.actionStatus, "cache is protected") || strings.Contains(m.actionStatus, "Released:") {
				t.Fatalf("failure was not preserved: status=%q err=%v", m.actionStatus, result.err)
			}
		})
	}
}
