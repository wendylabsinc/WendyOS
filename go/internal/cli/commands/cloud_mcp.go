package commands

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newCloudMCPCmd() *cobra.Command {
	var cloudGRPC string
	var allowTools []string
	var allowRead bool
	var allowAll bool
	var autoAccept bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Let a web chat (ChatGPT, Claude) use Wendy through this session",
		Long: "Open a session with the hosted Wendy MCP relay and serve the wendy MCP tools to a web chat you pair with it.\n" +
			"Every call runs here and is signed with your operator key; the chat can do nothing once this command exits.\n" +
			"Every permitted tool call requires confirmation by default. Use --allow-read to automatically approve read-only device and Cloud tools that do not access the host.\n" +
			"Use --allow-tool to permit other tools with confirmation, or --allow-all to permit every tool without per-call confirmation.\n" +
			"Pairing requires your acceptance unless --auto-accept is set for ChatGPT. That flag only accepts ChatGPT's verified OAuth client ID; other clients still need confirmation. A terminal is required for interactive confirmations.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := wendymcp.CheckToolNames(allowTools); err != nil {
				return fmt.Errorf("--allow-tool: %w", err)
			}
			if !isatty.IsTerminal(os.Stdin.Fd()) {
				return fmt.Errorf("wendy cloud mcp needs a terminal on stdin for pairing and tool confirmations")
			}
			// OpenRelaySession goes out unmarked, whatever this process inherited.
			if err := cloudrequest.SetChannel(cloudpbv2.RequestChannel_REQUEST_CHANNEL_UNSPECIFIED, "", ""); err != nil {
				return err
			}
			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			if err := wendymcp.RealmAllowsMCP(auth); err != nil {
				return err
			}
			srv, err := newMCPServer("", []string{"core"})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			stream, opened, closeConn, err := openRelaySession(ctx, auth)
			if err != nil {
				return err
			}
			defer closeConn()
			term := newPromptTerminal(ctx, os.Stdin, cmd.ErrOrStderr())
			stopWatchingTerminal := context.AfterFunc(term.ctx, stop)
			defer stopWatchingTerminal()
			err = serveCloudMCP(ctx, stream, opened, term, srv, wendymcp.BindingPolicy{AllowRead: allowRead, AllowAll: allowAll, Tools: allowTools, Session: auth}, autoAccept)
			return errors.Join(err, term.close())
		},
	}
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	cmd.Flags().StringSliceVar(&allowTools, "allow-tool", nil, "Also allow these tools by name (comma-separated); state changes and host access require confirmation")
	cmd.Flags().BoolVar(&allowRead, "allow-read", false, "Automatically approve read-only device and Cloud tools that do not access the host")
	cmd.Flags().BoolVar(&allowAll, "allow-all", false, "Allow every tool without per-call confirmation; does not change pairing approval")
	cmd.Flags().BoolVar(&autoAccept, "auto-accept", false, "Automatically accept ChatGPT pairing; tool permissions and per-call confirmations are unchanged")
	return cmd
}

type relayStream = cloudpbv2.RelayMcpService_RelaySessionClient

// The relay supplies the OAuth client ID verified from the web client's token.
// Match the exact public metadata document URL, never its display name or host.
const chatGPTRelayClientID = "https://chatgpt.com/oauth/client.json"

// openRelaySession opens a Relay MCP session (docs/relay-mcp.md in
// service-protos): the operator-signed OpenRelaySession, then the session's
// one stream, attached with its session_id. The stream lives with ctx. Both go
// to the tunnel broker, where Cloud serves RelayMcpService, not the API host.
func openRelaySession(ctx context.Context, auth *config.AuthConfig) (relayStream, *cloudpbv2.OpenRelaySessionResponse, func(), error) {
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return nil, nil, nil, fmt.Errorf("wendy cloud mcp needs an operator session; run 'wendy auth login'")
	}
	if auth.OAuthIssuer != "" {
		// The broker dial authenticates with a copy aimed at the broker host,
		// which cannot find its config entry to refresh: refresh this one first.
		if err := ensureOAuthAccessToken(ctx, auth); err != nil {
			return nil, nil, nil, err
		}
	}
	endpoint := relayBrokerEndpoint(auth.CloudGRPC)
	conn, tokenCtx, err := dialLiteCloudBroker(ctx, auth, endpoint, &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, nil, nil, err
	}
	opened := &cloudpbv2.OpenRelaySessionResponse{}
	err = cloudrequest.Invoke(tokenCtx, conn, auth, cloudpbv2.RelayMcpService_OpenRelaySession_FullMethodName,
		&cloudpbv2.OpenRelaySessionRequest{OrganizationId: auth.Certificates[0].TenantUUID()}, opened)
	if err == nil {
		var stream relayStream
		if stream, err = cloudpbv2.NewRelayMcpServiceClient(conn).RelaySession(tokenCtx); err == nil {
			if err = stream.Send(&cloudpbv2.RelaySessionRequest{Msg: &cloudpbv2.RelaySessionRequest_SessionId{SessionId: opened.GetSessionId()}}); err == nil {
				return stream, opened, func() { _ = conn.Close() }, nil
			}
		}
	}
	_ = conn.Close()
	switch status.Code(err) {
	case codes.PermissionDenied:
		return nil, nil, nil, fmt.Errorf("Wendy Cloud refused the relay session; your organization must allow MCP and Hosted MCP: %w", err)
	case codes.Unimplemented:
		return nil, nil, nil, fmt.Errorf("this Wendy Cloud does not run the MCP relay yet (%s): %w", endpoint, err)
	}
	return nil, nil, nil, fmt.Errorf("opening the relay session at %s: %w", endpoint, err)
}

// relayBrokerEndpoint is the tunnel broker's gRPC listener (tunnel.<env>:443)
// for a hosted dev or prod Cloud gRPC host. Self-hosted and local Clouds keep
// their own gRPC host.
func relayBrokerEndpoint(cloudGRPC string) string {
	host, _, _ := net.SplitHostPort(cloudGRPC)
	switch cloudrelay.CanonicalAPIHost(host) {
	case "api.dev.wendy.sh":
		return "https://tunnel.dev.wendy.dev:443"
	case "api.wendy.sh", "api.wendy.dev":
		return "https://tunnel.wendy.dev:443"
	}
	if clouddefaults.UsesPublicCA(cloudGRPC) {
		return "https://" + cloudGRPC
	}
	return "http://" + cloudGRPC
}

// serveCloudMCP asks about each web client that wants to bind, unless ChatGPT
// auto-accept is enabled. It serves the first accepted client until the relay
// ends the session or ctx ends (Ctrl-C). Every cloud call
// made meanwhile carries channel HOSTED_MCP with the client and session ids.
// Strings from the relay are untrusted: they reach the terminal only through
// printable, so none can rewrite a question the user answers.
// ponytail: one binding per session, later pairings are denied; serve several
// when the desktop app needs it.
func serveCloudMCP(parent context.Context, stream relayStream, opened *cloudpbv2.OpenRelaySessionResponse, term *promptTerminal, srv *wendymcp.Server, policy wendymcp.BindingPolicy, autoAccept bool) (result error) {
	ctx, cancel := context.WithCancel(parent)
	relay := &relayConn{stream: stream}
	pairings := make(chan *cloudpbv2.PairingRequested)
	received := make(chan error, 1)
	go func() { received <- relay.receive(ctx, pairings) }()
	var served chan error
	defer func() {
		// The RPC and this server are children of the same session context.
		// Cancellation can reach the RPC first, so consult their parent before
		// our own cleanup cancellation would hide an unrelated relay failure.
		if parent.Err() != nil {
			result = nil
		}
		// Every exit stops the server and waits for it to close what it
		// opened (tunnels) before the stream and the process go.
		cancel()
		relay.closeInput()
		if served != nil {
			<-served
		}
		_ = stream.CloseSend()
		term.printf("Session closed.\n")
	}()

	// Pin the session to the relay's reviewed catalog. Newer CLI tools stay
	// unavailable until the relay advertises them; unknown catalogs fail closed.
	manifest, manifestErr := wendymcp.ResolveRelayManifest(opened.GetManifestVersion())
	if manifestErr != nil {
		term.printf("Warning: %v. Every pairing request will be denied.\n", manifestErr)
	} else {
		policy.RelayManifest = manifest
	}
	if autoAccept {
		term.printf("Waiting for a web chat to pair. ChatGPT pairing is accepted automatically (--auto-accept); other clients require confirmation. Tool permissions are unchanged. Press Ctrl-C to end the session.\n")
	} else {
		term.printf("Waiting for a web chat to pair: use the Wendy connector there, then check that the code it shows matches the one asked about here. Press Ctrl-C to end the session.\n")
	}
	var client string
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-received:
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("relay session: %w", err)
		case err := <-served:
			served = nil
			return err
		case p := <-pairings:
			name, id, code := printable(p.GetClientName()), printable(p.GetClientId()), printable(p.GetCode())
			switch {
			case manifestErr != nil:
				term.printf("Denied %q: this CLI cannot serve the relay's published tool set.\n", name)
			case served != nil:
				term.printf("Denied %q: this session already serves %q.\n", name, client)
			case autoAccept && p.GetClientId() == chatGPTRelayClientID || term.askPairing(ctx, name, id, code):
				if autoAccept && p.GetClientId() == chatGPTRelayClientID {
					term.printf("Accepted ChatGPT pairing automatically (--auto-accept): %s.\n", id)
				}
				if err := cloudrequest.SetChannel(cloudpbv2.RequestChannel_REQUEST_CHANNEL_HOSTED_MCP, p.GetClientId(), opened.GetSessionId()); err != nil {
					return err
				}
				in, err := relay.bind(p.GetBindingId())
				if err != nil {
					return err
				}
				client = name
				policy.Confirm = func(ctx context.Context, tool string, args map[string]any) bool {
					shown, _ := json.Marshal(args)
					return term.ask(ctx, fmt.Sprintf("%q wants to run %s %s. Allow?", client, printable(tool), printable(string(shown))))
				}
				policy.Log = func(tool, outcome string) {
					term.printf("%s  %q  %s  %s\n", time.Now().Format("15:04:05"), client, printable(tool), printable(outcome))
				}
				if policy.AllowAll {
					term.printf("Serving %q for this signed-in connection. All tool calls run without confirmation (--allow-all).\n", client)
				} else if policy.AllowRead {
					term.printf("Serving %q for this signed-in connection. Read-only device and Cloud calls run without confirmation (--allow-read). State changes and host access require confirmation.\n", client)
				} else {
					term.printf("Serving %q for this signed-in connection. Every permitted tool call requires confirmation.\n", client)
				}
				served = make(chan error, 1)
				go func() {
					served <- srv.Serve(ctx, &policy, in, &frameWriter{relay: relay, binding: p.GetBindingId()})
				}()
				continue
			default:
				term.printf("Denied %q.\n", name)
			}
			if ctx.Err() != nil {
				return nil
			}
			if err := relay.send(&cloudpbv2.RelaySessionRequest{Msg: &cloudpbv2.RelaySessionRequest_PairingDecision{
				PairingDecision: &cloudpbv2.PairingDecision{BindingId: p.GetBindingId()},
			}}); err != nil {
				return fmt.Errorf("relay session: %w", err)
			}
		}
	}
}

func (t *promptTerminal) askPairing(ctx context.Context, name, id, code string) bool {
	renderer := lipgloss.NewRenderer(t.out)
	codeStyle := renderer.NewStyle().
		Bold(true).
		Foreground(tui.ColorPrimary).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorBorder).
		Padding(0, 2)
	t.printf("\n%q wants to pair\n%s\n\nPairing code\n%s\n\nCheck that this code matches the one in your web chat.\n",
		name, renderer.NewStyle().Foreground(tui.ColorDim).Render(id), codeStyle.Render(code))
	return t.ask(ctx, "Accept this signed-in connection across chats until this command exits?")
}

// printable replaces every rune a terminal could act on (controls, escape
// sequences, bidi overrides) so relay-supplied text displays as itself.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '\uFFFD'
	}, s)
}

// relayConn is the CLI end of the session stream. Sends are serialized (a
// gRPC stream allows one sender); frames carry per-sender sequence numbers
// from 1, and a received gap or repeat ends the session.
type relayConn struct {
	stream relayStream

	sendMu sync.Mutex
	sent   uint64

	mu      sync.Mutex
	binding string // the accepted binding; frames for any other are dropped
	in      *io.PipeWriter
	out     *io.PipeReader
}

func (r *relayConn) send(req *cloudpbv2.RelaySessionRequest) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if f := req.GetFrame(); f != nil {
		r.sent++
		f.Sequence = r.sent
	}
	return r.stream.Send(req)
}

// bind accepts binding and returns the reader its MCP messages arrive on.
func (r *relayConn) bind(binding string) (io.Reader, error) {
	out, in := io.Pipe()
	r.mu.Lock()
	r.binding, r.in, r.out = binding, in, out
	r.mu.Unlock()
	err := r.send(&cloudpbv2.RelaySessionRequest{Msg: &cloudpbv2.RelaySessionRequest_PairingDecision{
		PairingDecision: &cloudpbv2.PairingDecision{BindingId: binding, Accept: true},
	}})
	if err != nil {
		return nil, fmt.Errorf("relay session: %w", err)
	}
	return out, nil
}

// closeInput ends the bound client's input, unblocking both the server
// reading it and receive writing to it.
func (r *relayConn) closeInput() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.in != nil {
		_ = r.in.Close()
		_ = r.out.Close()
	}
}

// receive routes the relay's messages until the stream ends: pairing requests
// to pairings, the accepted binding's frames to its reader.
func (r *relayConn) receive(ctx context.Context, pairings chan<- *cloudpbv2.PairingRequested) error {
	var last uint64
	for {
		msg, err := r.stream.Recv()
		if err != nil {
			return err
		}
		if p := msg.GetPairingRequested(); p != nil {
			select {
			case pairings <- p:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		f := msg.GetFrame()
		if f == nil {
			continue
		}
		if f.GetSequence() != last+1 {
			return fmt.Errorf("relay frame %d after %d: out of sequence", f.GetSequence(), last)
		}
		last = f.GetSequence()
		r.mu.Lock()
		binding, in := r.binding, r.in
		r.mu.Unlock()
		if in == nil || f.GetBindingId() != binding {
			continue
		}
		if _, err := in.Write(append(f.GetMessage(), '\n')); err != nil {
			return err
		}
	}
}

// frameWriter sends each newline-terminated MCP message the server writes as
// one frame to binding.
type frameWriter struct {
	relay   *relayConn
	binding string
	buf     []byte
}

func (w *frameWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		msg := bytes.Clone(w.buf[:i])
		w.buf = w.buf[i+1:]
		if err := w.relay.send(&cloudpbv2.RelaySessionRequest{Msg: &cloudpbv2.RelaySessionRequest_Frame{
			Frame: &cloudpbv2.McpFrame{BindingId: w.binding, Message: msg},
		}}); err != nil {
			return 0, err
		}
	}
}
