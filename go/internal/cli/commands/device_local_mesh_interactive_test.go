package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestLocalMeshConfigureInteractive(t *testing.T) {
	failure := errors.New("device unavailable")
	current := &pb.LocalMeshConfiguration{Participate: true, Nan: true, Ethernet: true}
	for _, test := range []struct {
		name                         string
		selected                     []string
		promptErr, readErr, writeErr error
		missingSettings              bool
		args                         []string
		json                         bool
		want                         *pb.ConfigureLocalMeshRequest
		wantErr                      error
	}{
		{name: "change every setting", selected: []string{"roam", "share-uplink", "ble", "infrastructure-wifi"}, want: &pb.ConfigureLocalMeshRequest{
			Participate: proto.Bool(false), Roam: proto.Bool(true), ShareUplink: proto.Bool(true), Nan: proto.Bool(false), Ble: proto.Bool(true), Ethernet: proto.Bool(false), InfrastructureWifi: proto.Bool(true),
		}},
		{name: "disable participation preserves carriers", selected: []string{"nan", "ethernet"}, want: &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(false)}},
		{name: "unchanged", selected: []string{"participate", "nan", "ethernet"}},
		{name: "cancelled", promptErr: tui.ErrCancelled, wantErr: ErrUserCancelled},
		{name: "read failed", readErr: failure, wantErr: failure},
		{name: "missing settings", missingSettings: true},
		{name: "save failed", selected: []string{"nan", "ethernet"}, want: &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(false)}, writeErr: failure, wantErr: failure},
		{name: "explicit flags", args: []string{"--ble=false"}, want: &pb.ConfigureLocalMeshRequest{Ble: proto.Bool(false)}},
		{name: "JSON explicit flags", json: true, args: []string{"--roam=true"}, want: &pb.ConfigureLocalMeshRequest{Roam: proto.Bool(true)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousResolve, previousPrompt := resolveLocalMeshTarget, runLocalMeshChecklist
			previousJSON, previousTerminal := jsonOutput, isInteractiveTerminalFn
			t.Cleanup(func() {
				resolveLocalMeshTarget, runLocalMeshChecklist = previousResolve, previousPrompt
				jsonOutput, isInteractiveTerminalFn = previousJSON, previousTerminal
			})
			jsonOutput = test.json
			isInteractiveTerminalFn = func() bool { return true }
			reads, writes, prompts, connections := 0, 0, 0, 0
			connection, err := grpc.NewClient("passthrough:///configure-agent",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithUnaryInterceptor(func(_ context.Context, method string, request, reply any, _ *grpc.ClientConn, _ grpc.UnaryInvoker, _ ...grpc.CallOption) error {
					switch method {
					case pb.WendyLocalMeshAdminService_GetLocalMeshStatus_FullMethodName:
						reads++
						if test.readErr != nil {
							return test.readErr
						}
						if !test.missingSettings {
							proto.Merge(reply.(proto.Message), &pb.LocalMeshStatus{Configured: current})
						}
					case pb.WendyLocalMeshAdminService_ConfigureLocalMesh_FullMethodName:
						writes++
						if !proto.Equal(request.(proto.Message), test.want) {
							t.Fatalf("update=%v, want %v", request, test.want)
						}
						return test.writeErr
					default:
						t.Fatalf("unexpected RPC %s", method)
					}
					return nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			resolveLocalMeshTarget = func(_ context.Context, options ...resolveOption) (*SelectedDevice, error) {
				connections++
				cfg := resolveConfig{}
				for _, option := range options {
					option(&cfg)
				}
				if cfg.nonInteractive || !cfg.suppressUpdateCheck || !cfg.disablePickerEnroll {
					t.Fatal("configure must allow selection and suppress updates and enrollment")
				}
				return &SelectedDevice{Agent: &grpcclient.AgentConnection{Conn: connection}}, nil
			}
			runLocalMeshChecklist = func(_ string, items []tui.ChecklistItem, _ ...tea.ProgramOption) ([]tui.ChecklistItem, error) {
				prompts++
				if reads != 1 || writes != 0 {
					t.Fatal("prompt must follow read and precede write")
				}
				initial := map[string]bool{"participate": true, "roam": false, "share-uplink": false, "nan": true, "ble": false, "ethernet": true, "infrastructure-wifi": false}
				if len(items) != len(initial) {
					t.Fatalf("items=%v", items)
				}
				for _, item := range items {
					want, ok := initial[item.Value]
					if !ok || item.Selected != want {
						t.Fatalf("incorrect initial setting %+v", item)
					}
					delete(initial, item.Value)
				}
				var selected []tui.ChecklistItem
				for _, key := range test.selected {
					selected = append(selected, tui.ChecklistItem{Value: key, Selected: true})
				}
				return selected, test.promptErr
			}
			cmd := newNetCmd()
			cmd.SetArgs(append([]string{"configure"}, test.args...))
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var output bytes.Buffer
			cmd.SetOut(&output)
			err = cmd.Execute()
			if test.missingSettings {
				if err == nil {
					t.Fatal("accepted missing saved settings")
				}
			} else if !errors.Is(err, test.wantErr) {
				t.Fatalf("err=%v, want %v", err, test.wantErr)
			}
			wantReads, wantPrompts, wantWrites := 1, 1, 0
			if len(test.args) > 0 {
				wantReads, wantPrompts = 0, 0
			}
			if test.readErr != nil || test.missingSettings {
				wantPrompts = 0
			}
			if test.want != nil {
				wantWrites = 1
			}
			if reads != wantReads || prompts != wantPrompts || writes != wantWrites || connections != 1 {
				t.Fatalf("reads=%d prompts=%d writes=%d connections=%d", reads, prompts, writes, connections)
			}
			if connection.GetState() != connectivity.Shutdown {
				t.Fatal("connection was not closed")
			}
			if test.name == "unchanged" && !strings.Contains(output.String(), "settings unchanged") {
				t.Fatalf("output=%s", output.String())
			}
		})
	}
}

func TestLocalMeshConfigureRequiresFlagsWithoutInteractiveOutput(t *testing.T) {
	for _, test := range []struct {
		name           string
		terminal, json bool
	}{
		{"redirected", false, false}, {"JSON", true, true}, {"JSON redirected", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousResolve, previousJSON, previousTerminal := resolveLocalMeshTarget, jsonOutput, isInteractiveTerminalFn
			t.Cleanup(func() {
				resolveLocalMeshTarget, jsonOutput, isInteractiveTerminalFn = previousResolve, previousJSON, previousTerminal
			})
			jsonOutput = test.json
			isInteractiveTerminalFn = func() bool { return test.terminal }
			resolveLocalMeshTarget = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
				t.Fatal("connected before validating noninteractive settings")
				return nil, nil
			}
			cmd := newNetCmd()
			cmd.SetArgs([]string{"configure"})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			if err := cmd.Execute(); !errors.Is(err, errNoLocalMeshSettings) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
