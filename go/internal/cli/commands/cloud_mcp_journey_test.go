package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Keep the denied and accepted journeys on one real gRPC stream. The next
// pairing decision is a barrier: a reply to the refused binding must never
// appear before it or consume a sequence number in the accepted session.
func TestCloudMCPPairingJourneyCatalogues(t *testing.T) {
	for _, version := range []string{"", "2026.10.13-212424+7d3624a60bf68a8c"} {
		name := version
		if name == "" {
			name = "current"
		}
		t.Run(name, func(t *testing.T) {
			opened := testOpened(t, version)
			catalog, err := wendymcp.ResolveRelayManifest(opened.ManifestVersion)
			if err != nil {
				t.Fatal(err)
			}
			if catalog == nil {
				t.Fatal("resolved catalogue is missing")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			answers := make(chan string, 2)
			answers <- "n"
			answers <- "y"
			var replies []*cloudpbv2.McpFrame
			done, out, _ := runRelaySessionWithTerminal(t, opened, wendymcp.BindingPolicy{AllowAll: true}, answers,
				func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
					for index, binding := range []string{"denied", "accepted"} {
						if err := stream.Send(pairing(binding, "ABCD-2345")); err != nil {
							return err
						}
						response, err := stream.Recv()
						if err != nil {
							return err
						}
						decision := response.GetPairingDecision()
						if decision == nil || decision.BindingId != binding || decision.Accept != (index == 1) {
							return fmt.Errorf("unexpected pairing response: %v", response)
						}
						if index == 0 {
							if err := stream.Send(frame(1, binding, `{"jsonrpc":"2.0","id":90,"method":"tools/list"}`)); err != nil {
								return err
							}
						}
					}
					// An old denied binding cannot reuse the new session's server.
					if err := stream.Send(frame(2, "denied", `{"jsonrpc":"2.0","id":91,"method":"tools/list"}`)); err != nil {
						return err
					}
					for index, message := range []string{
						`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`,
						`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
						`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wendy_status","arguments":{}}}`,
					} {
						if err := stream.Send(frame(uint64(index+3), "accepted", message)); err != nil {
							return err
						}
						response, err := stream.Recv()
						if err != nil {
							return err
						}
						reply := response.GetFrame()
						if reply == nil || reply.BindingId != "accepted" || reply.Sequence != uint64(index+1) {
							return fmt.Errorf("unexpected accepted-session response: %v", response)
						}
						replies = append(replies, reply)
					}
					return nil
				}, relaySessionOptions{ctx: ctx})
			if err := <-done; err != nil {
				t.Fatalf("pairing journey: %v\n%s", err, out.String())
			}
			if len(replies) != 3 {
				t.Fatalf("got %d replies, want initialize, list and status", len(replies))
			}
			for index, reply := range replies {
				var message struct {
					ID     int             `json:"id"`
					Error  json.RawMessage `json:"error"`
					Result struct {
						ProtocolVersion string           `json:"protocolVersion"`
						Tools           []map[string]any `json:"tools"`
						IsError         bool             `json:"isError"`
						Content         []any            `json:"content"`
					} `json:"result"`
				}
				if err := json.Unmarshal(reply.Message, &message); err != nil || message.ID != index+1 || len(message.Error) > 0 {
					t.Fatalf("invalid RPC response %s: %v", reply.Message, err)
				}
				switch index {
				case 0:
					if message.Result.ProtocolVersion != "2025-06-18" {
						t.Fatalf("initialize: %s", reply.Message)
					}
				case 1:
					data, err := json.Marshal(catalog.Tools)
					if err != nil {
						t.Fatal(err)
					}
					var expected []map[string]any
					if err := json.Unmarshal(data, &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(message.Result.Tools, expected) {
						t.Fatal("served tools, titles or schemas differ from the published catalogue")
					}
				case 2:
					if message.Result.IsError || len(message.Result.Content) == 0 {
						t.Fatalf("accepted status call failed: %s", reply.Message)
					}
				}
			}
			if strings.Count(out.String(), "wants to pair") != 2 || strings.Contains(out.String(), "wants to run") {
				t.Fatalf("unexpected confirmation flow:\n%s", out.String())
			}
		})
	}
}
