package commands

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeRelay plays the hosted relay's side of RelaySession with a per-test
// script.
type fakeRelay struct {
	cloudpbv2.UnimplementedRelayMcpServiceServer
	script func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error
}

func (f *fakeRelay) RelaySession(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
	first, err := stream.Recv()
	if err != nil || first.GetSessionId() != "sess-1" {
		return err
	}
	return f.script(stream)
}

// testOpened is a session whose relay publishes this CLI's tool manifest.
func testOpened(t *testing.T, manifestVersion string) *cloudpbv2.OpenRelaySessionResponse {
	t.Helper()
	if manifestVersion == "" {
		m, err := wendymcp.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		manifestVersion = m.Version
	}
	return &cloudpbv2.OpenRelaySessionResponse{SessionId: "sess-1", ManifestVersion: manifestVersion}
}

// runRelaySession serves one relay session against script. The user's
// answers go to answers; they count as typed after the question.
func runRelaySession(t *testing.T, opened *cloudpbv2.OpenRelaySessionResponse, policy wendymcp.BindingPolicy, answers <-chan string, script func(cloudpbv2.RelayMcpService_RelaySessionServer) error) (<-chan error, *lockedBuffer) {
	done, out, _ := runRelaySessionWithTerminal(t, opened, policy, answers, script, relaySessionOptions{})
	return done, out
}

type relaySessionOptions struct {
	ctx        context.Context
	input      io.Reader
	typed      io.Writer
	server     *wendymcp.Server
	autoAccept bool
}

func runRelaySessionWithTerminal(t *testing.T, opened *cloudpbv2.OpenRelaySessionResponse, policy wendymcp.BindingPolicy, answers <-chan string, script func(cloudpbv2.RelayMcpService_RelaySessionServer) error, opts relaySessionOptions) (<-chan error, *lockedBuffer, *promptTerminal) {
	t.Helper()
	for _, env := range []string{"WENDY_MCP_CHANNEL", "WENDY_MCP_CLIENT_ID", "WENDY_MCP_RELAY_SESSION_ID"} {
		t.Setenv(env, "")
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	cloudpbv2.RegisterRelayMcpServiceServer(srv, &fakeRelay{script: script})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///relay",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if opts.ctx == nil {
		opts.ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(opts.ctx)
	t.Cleanup(cancel)
	stream, err := cloudpbv2.NewRelayMcpServiceClient(conn).RelaySession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&cloudpbv2.RelaySessionRequest{Msg: &cloudpbv2.RelaySessionRequest_SessionId{SessionId: "sess-1"}}); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	input, typed := opts.input, opts.typed
	if input == nil {
		reader, writer := io.Pipe()
		t.Cleanup(func() { _ = reader.Close() })
		t.Cleanup(func() { _ = writer.Close() })
		input, typed = reader, writer
	}
	prompts := make(chan struct{}, 1)
	term := newPromptTerminal(ctx, input, &relayPromptOutput{out: out, prompts: prompts})
	stopWatchingTerminal := context.AfterFunc(term.ctx, cancel)
	t.Cleanup(func() { stopWatchingTerminal() })
	t.Cleanup(func() { _ = term.close() })
	go func() {
		for {
			select {
			case <-prompts:
			case <-term.ctx.Done():
				return
			}
			select {
			case a := <-answers:
				if _, err := io.WriteString(typed, a); err != nil {
					return
				}
			case <-term.ctx.Done():
				return
			}
		}
	}()
	done := make(chan error, 1)
	if opts.server == nil {
		opts.server = wendymcp.New(&config.Config{}, nil)
	}
	go func() {
		err := serveCloudMCP(ctx, stream, opened, term, opts.server, policy, opts.autoAccept)
		_ = term.close()
		done <- err
	}()
	return done, out, term
}

func TestCloudMCPAcceptedSessionStopsOnCtrlC(t *testing.T) {
	for _, interrupt := range []string{"key", "signal"} {
		t.Run(interrupt, func(t *testing.T) { testCloudMCPAcceptedSessionInterrupt(t, interrupt) })
	}
}

func TestCloudMCPReportsRelayFailureWithoutCallerCancellation(t *testing.T) {
	for _, code := range []codes.Code{codes.Canceled, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			done, _ := runRelaySession(t, testOpened(t, ""), wendymcp.BindingPolicy{}, nil, func(cloudpbv2.RelayMcpService_RelaySessionServer) error {
				return status.Error(code, "relay ended unexpectedly")
			})
			select {
			case err := <-done:
				if status.Code(err) != code {
					t.Fatalf("relay failure = %v; want %v", err, code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("relay failure did not stop the session")
			}
		})
	}
}

func testCloudMCPAcceptedSessionInterrupt(t *testing.T, interrupt string) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_NO_UPDATE_CHECK", "1")
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_DEVICE", "")
	t.Setenv("WENDY_AGENT_SOCKET", "")
	server, err := newMCPServer("", []string{"core"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	t.Cleanup(stop)
	keyboard, tty, err := pty.Open()
	if errors.Is(err, pty.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyboard.Close() })
	t.Cleanup(func() { _ = tty.Close() })
	answers := make(chan string, 1)
	answers <- "y"
	ready := make(chan struct{})
	done, out, _ := runRelaySessionWithTerminal(t, testOpened(t, ""), wendymcp.BindingPolicy{}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(frame(1, "b1", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)); err != nil {
			return err
		}
		if _, err := stream.Recv(); err != nil {
			return err
		}
		close(ready)
		<-stream.Context().Done()
		return stream.Context().Err()
	}, relaySessionOptions{ctx: ctx, input: tty, typed: keyboard, server: server})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted MCP server did not initialize")
	}
	if interrupt == "signal" {
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := keyboard.Write([]byte{3}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ctrl-C returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not stop the accepted relay session")
	}
	if !strings.Contains(out.String(), "Session closed.") {
		t.Fatalf("missing shutdown message: %s", out.String())
	}
}

// Answer only after the real picker has been rendered, as a terminal user would.
type relayPromptOutput struct {
	mu      sync.Mutex
	out     *lockedBuffer
	prompts chan<- struct{}
	shown   int
}

func (w *relayPromptOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.out.Write(p)
	if shown := strings.Count(w.out.String(), "(y/n)"); shown > w.shown {
		w.shown = shown
		w.prompts <- struct{}{}
	}
	return n, err
}

func pairing(binding, code string) *cloudpbv2.RelaySessionResponse {
	return &cloudpbv2.RelaySessionResponse{Msg: &cloudpbv2.RelaySessionResponse_PairingRequested{
		PairingRequested: &cloudpbv2.PairingRequested{BindingId: binding, ClientName: "ChatGPT", ClientId: "https://chatgpt.com/oauth", Code: code},
	}}
}

func frame(seq uint64, binding, msg string) *cloudpbv2.RelaySessionResponse {
	return &cloudpbv2.RelaySessionResponse{Msg: &cloudpbv2.RelaySessionResponse_Frame{
		Frame: &cloudpbv2.McpFrame{Sequence: seq, BindingId: binding, Message: []byte(msg)},
	}}
}

func TestCloudMCPDeniedPairingBindsNothing(t *testing.T) {
	answers := make(chan string, 2)
	answers <- "n"
	answers <- "n"
	var decisions []*cloudpbv2.PairingDecision
	done, out := runRelaySession(t, testOpened(t, ""), wendymcp.BindingPolicy{AllowAll: true}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		// The relay mints the code; the user checks it against the chat's.
		for _, p := range []*cloudpbv2.RelaySessionResponse{pairing("b0", "WRONG-0000"), pairing("b1", "ABCD-2345")} {
			if err := stream.Send(p); err != nil {
				return err
			}
			got, err := stream.Recv()
			if err != nil {
				return err
			}
			decisions = append(decisions, got.GetPairingDecision())
		}
		// Frames for a binding the user did not accept go nowhere.
		return stream.Send(frame(1, "b1", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	})
	if err := <-done; err != nil {
		t.Fatalf("serveCloudMCP = %v, want a clean end when the relay closes", err)
	}
	if len(decisions) != 2 || decisions[0].GetAccept() || decisions[1].GetAccept() || decisions[1].GetBindingId() != "b1" {
		t.Fatalf("decisions %v, want b0 and b1 both denied", decisions)
	}
	if log := out.String(); strings.Count(log, "wants to pair") != 2 || !strings.Contains(log, "Pairing code") || !strings.Contains(log, "ABCD-2345") || !strings.Contains(log, "Accept this signed-in connection across chats until this command exits?") {
		t.Fatalf("want each pairing asked about with its code:\n%s", log)
	}
}

// A relay that showed the chat an unknown tool set (or none) gets no pairing,
// and the user is never asked.
func TestCloudMCPManifestMismatchDeniesPairing(t *testing.T) {
	var decision *cloudpbv2.PairingDecision
	done, out := runRelaySession(t, testOpened(t, "0.0.0+0000000000000000"), wendymcp.BindingPolicy{AllowAll: true}, nil, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		got, err := stream.Recv()
		decision = got.GetPairingDecision()
		return err
	})
	if err := <-done; err != nil {
		t.Fatalf("serveCloudMCP = %v", err)
	}
	if decision == nil || decision.GetAccept() || decision.GetBindingId() != "b1" {
		t.Fatalf("decision %v, want b1 denied", decision)
	}
	if log := out.String(); !strings.Contains(log, "Warning:") || strings.Contains(log, "wants to pair") {
		t.Fatalf("want a warning and no question:\n%s", log)
	}
}

func TestCloudMCPAutoAcceptMatchesExactChatGPTIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, clientID, displayName, manifest string
		autoAccept, accepted, prompted        bool
	}{
		{name: "ChatGPT", clientID: chatGPTRelayClientID, displayName: "ChatGPT", autoAccept: true, accepted: true},
		{name: "identity does not depend on display name", clientID: chatGPTRelayClientID, displayName: "chatgpt.com", autoAccept: true, accepted: true},
		{name: "default requires confirmation", clientID: chatGPTRelayClientID, displayName: "ChatGPT", prompted: true},
		{name: "spoofed display name", clientID: "https://example.com/oauth/client.json", displayName: "ChatGPT", autoAccept: true, prompted: true},
		{name: "same host different path", clientID: "https://chatgpt.com/oauth", displayName: "ChatGPT", autoAccept: true, prompted: true},
		{name: "query suffix", clientID: chatGPTRelayClientID + "?client=other", displayName: "ChatGPT", autoAccept: true, prompted: true},
		{name: "lookalike host", clientID: "https://chatgpt.com.example.com/oauth/client.json", displayName: "ChatGPT", autoAccept: true, prompted: true},
		{name: "missing identity", displayName: "ChatGPT", autoAccept: true, prompted: true},
		{name: "other trusted vendor", clientID: "https://claude.ai/oauth/mcp-oauth-client-metadata", displayName: "Claude", autoAccept: true, prompted: true},
		{name: "incompatible catalog", clientID: chatGPTRelayClientID, displayName: "ChatGPT", manifest: "0.0.0+0000000000000000", autoAccept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			answers := make(chan string, 1)
			if tc.prompted {
				answers <- "n"
			}
			var decision *cloudpbv2.PairingDecision
			done, out, _ := runRelaySessionWithTerminal(t, testOpened(t, tc.manifest), wendymcp.BindingPolicy{}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
				request := pairing("b1", "ABCD-2345")
				request.GetPairingRequested().ClientId = tc.clientID
				request.GetPairingRequested().ClientName = tc.displayName
				if err := stream.Send(request); err != nil {
					return err
				}
				got, err := stream.Recv()
				decision = got.GetPairingDecision()
				return err
			}, relaySessionOptions{ctx: ctx, autoAccept: tc.autoAccept})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if decision == nil || decision.GetAccept() != tc.accepted || decision.GetBindingId() != "b1" {
				t.Fatalf("decision = %v, want accepted=%v\n%s", decision, tc.accepted, out.String())
			}
			log := out.String()
			if got := strings.Contains(log, "wants to pair"); got != tc.prompted {
				t.Fatalf("pairing prompt = %v, want %v\n%s", got, tc.prompted, log)
			}
			if got := strings.Contains(log, "Accepted ChatGPT pairing automatically"); got != tc.accepted {
				t.Fatalf("automatic acceptance log = %v, want %v\n%s", got, tc.accepted, log)
			}
		})
	}
}

func TestCloudMCPAutoAcceptKeepsToolPermissionsAndOneBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answers := make(chan string, 2)
	answers <- "n" // Refuse the read; automatic pairing does not approve calls.
	answers <- "n" // Refuse the allowed mutation.
	var decisions []*cloudpbv2.PairingDecision
	var replies []*cloudpbv2.McpFrame
	done, out, _ := runRelaySessionWithTerminal(t, testOpened(t, ""), wendymcp.BindingPolicy{Tools: []string{"device_disconnect"}}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		for _, binding := range []string{"b1", "b2"} {
			request := pairing(binding, "ABCD-2345")
			request.GetPairingRequested().ClientId = chatGPTRelayClientID
			if err := stream.Send(request); err != nil {
				return err
			}
			got, err := stream.Recv()
			if err != nil {
				return err
			}
			decisions = append(decisions, got.GetPairingDecision())
		}
		for seq, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wendy_status","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_list","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"device_disconnect","arguments":{}}}`,
		} {
			if err := stream.Send(frame(uint64(seq+1), "b1", msg)); err != nil {
				return err
			}
			reply, err := stream.Recv()
			if err != nil {
				return err
			}
			replies = append(replies, reply.GetFrame())
		}
		return nil
	}, relaySessionOptions{ctx: ctx, autoAccept: true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || !decisions[0].GetAccept() || decisions[1].GetAccept() || decisions[1].GetBindingId() != "b2" {
		t.Fatalf("want only the first binding accepted, got %v\n%s", decisions, out.String())
	}
	if len(replies) != 3 {
		t.Fatalf("got %d tool replies, want three\n%s", len(replies), out.String())
	}
	for _, reply := range replies {
		var call struct {
			Result struct {
				StructuredContent map[string]any `json:"structuredContent"`
			}
		}
		if err := json.Unmarshal(reply.GetMessage(), &call); err != nil || call.Result.StructuredContent["error_code"] != "DENIED" {
			t.Fatalf("want DENIED from preserved permissions, got %s (%v)", reply.GetMessage(), err)
		}
	}
	log := out.String()
	if strings.Contains(log, "wants to pair") || strings.Count(log, "wants to run") != 2 {
		t.Fatalf("want read and mutation confirmation prompts:\n%s", log)
	}
	for _, want := range []string{"Every permitted tool call requires confirmation.", "this session already serves", "wendy_status  refused by the user", "device_list  refused: not allowed for this client", "device_disconnect  refused by the user"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q:\n%s", want, log)
		}
	}
}

func TestCloudMCPCompatibleRelayPinsPublishedTools(t *testing.T) {
	answers := make(chan string, 1)
	answers <- "y"
	var accepted *cloudpbv2.PairingDecision
	var replies []*cloudpbv2.McpFrame
	done, out := runRelaySession(t, testOpened(t, "2026.10.13-212424+7d3624a60bf68a8c"), wendymcp.BindingPolicy{AllowAll: true}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		got, err := stream.Recv()
		if err != nil {
			return err
		}
		accepted = got.GetPairingDecision()
		for seq, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"simulator_start","arguments":{"name":"dev"}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"simulator_create","arguments":{"name":"dev","profile":"wendy-lite"}}}`,
		} {
			if err := stream.Send(frame(uint64(seq+1), "b1", msg)); err != nil {
				return err
			}
			reply, err := stream.Recv()
			if err != nil {
				return err
			}
			replies = append(replies, reply.GetFrame())
		}
		return nil
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !accepted.GetAccept() || len(replies) != 3 {
		t.Fatalf("compatible pairing did not serve tools: accepted=%v replies=%d", accepted, len(replies))
	}
	var listed struct {
		Result struct{ Tools []json.RawMessage }
	}
	if err := json.Unmarshal(replies[0].GetMessage(), &listed); err != nil || len(listed.Result.Tools) != 58 || strings.Contains(string(replies[0].GetMessage()), `"simulator_start"`) {
		t.Fatalf("session did not pin the older 58-tool catalog: %s (%v)", replies[0].GetMessage(), err)
	}
	for i, want := range []string{"NOT_FOUND", "INVALID_ARGUMENT"} {
		var call struct {
			Result struct {
				IsError           bool           `json:"isError"`
				StructuredContent map[string]any `json:"structuredContent"`
			}
		}
		if err := json.Unmarshal(replies[i+1].GetMessage(), &call); err != nil || !call.Result.IsError || call.Result.StructuredContent["error_code"] != want {
			t.Fatalf("unpublished call replied %s, want %s (%v)", replies[i+1].GetMessage(), want, err)
		}
	}
	if log := out.String(); strings.Contains(log, "Warning:") || strings.Contains(log, "wants to run") {
		t.Fatalf("compatible catalog warned or unpublished call reached confirmation:\n%s", log)
	}
}

func TestCloudMCPAcceptedClientNeedsConfirmationToMutate(t *testing.T) {
	answers := make(chan string, 2)
	answers <- "y" // accept the pairing
	answers <- "n" // refuse the call
	var accepted *cloudpbv2.PairingDecision
	var replies []*cloudpbv2.McpFrame
	done, out := runRelaySession(t, testOpened(t, ""), wendymcp.BindingPolicy{Tools: []string{"device_disconnect"}}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		got, err := stream.Recv()
		if err != nil {
			return err
		}
		accepted = got.GetPairingDecision()
		// A frame for a binding the user never accepted is dropped.
		if err := stream.Send(frame(1, "b9", `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)); err != nil {
			return err
		}
		for seq, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"chatgpt","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_disconnect","arguments":{}}}`,
		} {
			if err := stream.Send(frame(uint64(seq+2), "b1", msg)); err != nil {
				return err
			}
			reply, err := stream.Recv()
			if err != nil {
				return err
			}
			replies = append(replies, reply.GetFrame())
		}
		return nil // the relay ends the session
	})
	if err := <-done; err != nil {
		t.Fatalf("serveCloudMCP = %v", err)
	}
	if !accepted.GetAccept() || accepted.GetBindingId() != "b1" {
		t.Fatalf("decision %v, want b1 accepted", accepted)
	}
	if os.Getenv("WENDY_MCP_CHANNEL") != "REQUEST_CHANNEL_HOSTED_MCP" || os.Getenv("WENDY_MCP_CLIENT_ID") != "https://chatgpt.com/oauth" || os.Getenv("WENDY_MCP_RELAY_SESSION_ID") != "sess-1" {
		t.Error("signed calls after the binding are not marked HOSTED_MCP with the client and session")
	}
	if len(replies) != 2 || replies[0].GetSequence() != 1 || replies[1].GetSequence() != 2 || replies[1].GetBindingId() != "b1" {
		t.Fatalf("reply frames %v, want sequences 1 and 2 for b1", replies)
	}
	var call struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
		}
	}
	if err := json.Unmarshal(replies[1].GetMessage(), &call); err != nil || call.Result.StructuredContent["error_code"] != "DENIED" {
		t.Fatalf("refused device_disconnect replied %s, want DENIED", replies[1].GetMessage())
	}
	log := out.String()
	for _, want := range []string{`"ChatGPT" wants to run device_disconnect {}`, `"ChatGPT"  device_disconnect  refused by the user`, "Session closed."} {
		if !strings.Contains(log, want) {
			t.Errorf("terminal lacks %q:\n%s", want, log)
		}
	}
}

func TestCloudMCPAllowReadOnlySkipsReadConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answers := make(chan string, 3)
	answers <- "y" // Pairing still needs approval.
	answers <- "n" // Refuse host access.
	answers <- "n" // Refuse the mutation.
	var replies []*cloudpbv2.McpFrame
	policy := wendymcp.BindingPolicy{AllowRead: true, Tools: []string{"device_list", "device_disconnect"}}
	done, out, _ := runRelaySessionWithTerminal(t, testOpened(t, ""), policy, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		if _, err := stream.Recv(); err != nil {
			return err
		}
		for seq, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wendy_status","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_list","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"device_disconnect","arguments":{}}}`,
		} {
			if err := stream.Send(frame(uint64(seq+1), "b1", msg)); err != nil {
				return err
			}
			reply, err := stream.Recv()
			if err != nil {
				return err
			}
			replies = append(replies, reply.GetFrame())
		}
		return nil
	}, relaySessionOptions{ctx: ctx})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(replies) != 3 {
		t.Fatalf("got %d tool replies, want three\n%s", len(replies), out.String())
	}
	for i, reply := range replies {
		var call struct {
			Result *struct {
				IsError           bool           `json:"isError"`
				StructuredContent map[string]any `json:"structuredContent"`
			}
		}
		if err := json.Unmarshal(reply.GetMessage(), &call); err != nil || call.Result == nil {
			t.Fatalf("invalid tool reply: %s (%v)", reply.GetMessage(), err)
		}
		if i == 0 && call.Result.IsError || i > 0 && call.Result.StructuredContent["error_code"] != "DENIED" {
			t.Fatalf("want successful read followed by denied host/mutation calls, got %s", reply.GetMessage())
		}
	}
	log := out.String()
	if strings.Count(log, "wants to pair") != 1 || strings.Count(log, "wants to run") != 2 || strings.Contains(log, "wants to run wendy_status") {
		t.Fatalf("--allow-read must ask only about pairing, host access and mutations:\n%s", log)
	}
	for _, want := range []string{"Read-only device and Cloud calls run without confirmation (--allow-read).", `"ChatGPT"  wendy_status  ok`, `"ChatGPT" wants to run device_list`, `"ChatGPT" wants to run device_disconnect`} {
		if !strings.Contains(log, want) {
			t.Errorf("terminal lacks %q:\n%s", want, log)
		}
	}
}

func TestCloudMCPAllowAllOnlyConfirmsPairing(t *testing.T) {
	answers := make(chan string, 1)
	answers <- "y" // The pairing is the only prompt the user answers.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var accepted *cloudpbv2.PairingDecision
	var replies []*cloudpbv2.McpFrame
	done, out, _ := runRelaySessionWithTerminal(t, testOpened(t, ""), wendymcp.BindingPolicy{AllowAll: true}, answers, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(pairing("b1", "ABCD-2345")); err != nil {
			return err
		}
		got, err := stream.Recv()
		if err != nil {
			return err
		}
		accepted = got.GetPairingDecision()
		for seq, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_list","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"device_disconnect","arguments":{}}}`,
		} {
			if err := stream.Send(frame(uint64(seq+1), "b1", msg)); err != nil {
				return err
			}
			reply, err := stream.Recv()
			if err != nil {
				return err
			}
			replies = append(replies, reply.GetFrame())
		}
		return nil
	}, relaySessionOptions{ctx: ctx})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !accepted.GetAccept() || len(replies) != 2 {
		t.Fatalf("--allow-all did not serve both calls after pairing: accepted=%v replies=%d\n%s", accepted, len(replies), out.String())
	}
	for _, reply := range replies {
		var call struct {
			Result *struct {
				IsError bool `json:"isError"`
			}
		}
		if err := json.Unmarshal(reply.GetMessage(), &call); err != nil || call.Result == nil || call.Result.IsError {
			t.Fatalf("--allow-all tool call failed: %s (%v)", reply.GetMessage(), err)
		}
	}
	log := out.String()
	if strings.Count(log, "wants to pair") != 1 || strings.Contains(log, "wants to run") {
		t.Fatalf("--allow-all must ask only about pairing:\n%s", log)
	}
	for _, want := range []string{"All tool calls run without confirmation (--allow-all).", `"ChatGPT"  device_list  ok`, `"ChatGPT"  device_disconnect  ok`} {
		if !strings.Contains(log, want) {
			t.Errorf("terminal lacks %q:\n%s", want, log)
		}
	}
}

func TestCloudMCPEndsOnFrameSequenceGap(t *testing.T) {
	done, _ := runRelaySession(t, testOpened(t, ""), wendymcp.BindingPolicy{}, nil, func(stream cloudpbv2.RelayMcpService_RelaySessionServer) error {
		if err := stream.Send(frame(2, "b1", `{}`)); err != nil {
			return err
		}
		<-stream.Context().Done()
		return nil
	})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "out of sequence") {
		t.Fatalf("serveCloudMCP = %v, want an out-of-sequence end", err)
	}
}

func TestPrintableNeutralizesTerminalControls(t *testing.T) {
	forged := "ChatGPT wants to run wendy_status {}. Allow? [y/N] \x1b[8m\u202e\x7f\u0085"
	if got := printable(forged); strings.ContainsAny(got, "\x1b\u202e\x7f\u0085") || !strings.HasPrefix(got, "ChatGPT wants") {
		t.Fatalf("printable(%q) = %q", forged, got)
	}
}

func TestMCPServeRefusesWhenRealmDisallowsMCP(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u","allow_mcp":false}`)) + ".sig"
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443", APIKey: token}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_MCP_CHANNEL", "")
	// `wendy mcp serve` starts here.
	if err := enterLocalMCPChannel(); err == nil || !strings.Contains(err.Error(), "Allow MCP") {
		t.Fatalf("enterLocalMCPChannel = %v, want the Allow MCP refusal", err)
	}
	if os.Getenv("WENDY_MCP_CHANNEL") != "" {
		t.Error("a refused MCP server marked the channel")
	}
}

func TestCloudMCPRefusesUnknownAllowTool(t *testing.T) {
	cmd := newCloudMCPCmd()
	cmd.SetArgs([]string{"--allow-tool", "device_disconnect,os_instal_verify"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `"os_instal_verify"`) || !strings.Contains(err.Error(), "os_install_verify") {
		t.Fatalf("Execute = %v, want the unknown name refused with the valid ones listed", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Cloud serves RelayMcpService only on the tunnel broker; the API host answers
// Unimplemented, so hosted dev and prod sessions must dial their tunnel broker.
func TestCloudMCPRelaySessionDialsTunnelBroker(t *testing.T) {
	for cloudGRPC, want := range map[string]string{
		"api.dev.wendy.sh:443":  "tunnel.dev.wendy.dev:443",
		"api.dev.wendy.dev:443": "tunnel.dev.wendy.dev:443",
		"api.wendy.sh:443":      "tunnel.wendy.dev:443",
		"api.wendy.dev:443":     "tunnel.wendy.dev:443",
		"cloud.example:443":     "cloud.example:443",
		"localhost:50051":       "localhost:50051",
	} {
		auth := &config.AuthConfig{CloudGRPC: cloudGRPC}
		conn, _, err := dialLiteCloudBroker(context.Background(), auth, relayBrokerEndpoint(cloudGRPC), &tls.Config{MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatalf("%s: %v", cloudGRPC, err)
		}
		if got := conn.Target(); got != want {
			t.Errorf("%s: relay session dials %s, want %s", cloudGRPC, got, want)
		}
		_ = conn.Close()
	}
}
