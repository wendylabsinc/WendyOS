//go:build js && wasm

// This experiment reuses Wendy's real client and TLS verifier in a browser.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall/js"
	"time"

	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type fixture struct {
	Certificate config.CertificateInfo `json:"certificate"`
}

func main() {
	// The experiment never reads or persists the operator's credentials.
	_ = os.Setenv("WENDY_TLS_SESSION_STORE", "off")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks, err := run(ctx)
	result := map[string]any{"ok": err == nil, "checks": checks}
	if err != nil {
		result["error"] = err.Error()
	}
	b, _ := json.Marshal(result)
	js.Global().Set("wasmResult", js.Global().Get("JSON").Call("parse", string(b)))
	js.Global().Get("document").Call("getElementById", "result").Set("textContent", string(b))
}

func run(ctx context.Context) ([]string, error) {
	checks := []string{}
	origin := js.Global().Get("location").Get("origin").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/fixture", nil)
	if err != nil {
		return checks, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checks, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checks, fmt.Errorf("fixture: %s", resp.Status)
	}
	var f fixture
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return checks, err
	}
	wsURL := "ws" + origin[len("http"):] + "/tunnel"

	// TunnelDialer keeps the stream alive after gRPC cancels its dial context,
	// and opens a new WebSocket for each reconnect. TLS stays above this layer.
	dial := clouddefaults.TunnelDialer(func(ctx context.Context) (net.Conn, error) {
		ws, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			return nil, err
		}
		return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
	})
	connect := func(cert config.CertificateInfo, opts ...grpc.DialOption) (*grpcclient.AgentConnection, error) {
		// Bypass DNS: only the WebSocket relay needs an actual network address.
		return grpcclient.ConnectWithTLSExpecting(ctx, "passthrough:///wasm-agent.test:443", &cert, nil, nil, append([]grpc.DialOption{dial}, opts...)...)
	}
	conn, err := connect(f.Certificate)
	if err != nil {
		return checks, err
	}
	defer conn.Close()
	version, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return checks, fmt.Errorf("unary: %w", err)
	}
	if version.Version != "wasm-mtls-fixture" {
		return checks, fmt.Errorf("unexpected version %q", version.Version)
	}
	checks = append(checks, "Wendy GetAgentVersion over mTLS")

	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	stream, err := conn.ShellService.HostShell(streamCtx)
	if err != nil {
		return checks, err
	}
	if err := stream.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Start_{Start: &agentpb.HostShellRequest_Start{}}}); err != nil {
		return checks, err
	}
	// Exchange replies before closing the send side. Include binary bytes and a
	// message larger than the HTTP/2 flow-control window to exercise framing.
	for _, payload := range [][]byte{[]byte("hello from WASM\x00\xff"), bytes.Repeat([]byte{0, 1, 127, 255}, 128*1024)} {
		if err := stream.Send(&agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_StdinData{StdinData: payload}}); err != nil {
			return checks, err
		}
		reply, err := stream.Recv()
		if err != nil {
			return checks, fmt.Errorf("bidi receive: %w", err)
		}
		if !bytes.Equal(reply.GetStdoutData(), payload) {
			return checks, fmt.Errorf("binary echo mismatch")
		}
	}
	if err := stream.CloseSend(); err != nil {
		return checks, err
	}
	if _, err := stream.Recv(); err != io.EOF {
		return checks, fmt.Errorf("stream final status: %v", err)
	}
	checks = append(checks, "bidirectional stream, binary data, 512 KiB payload, clean EOF")

	// Force transport loss, then reuse the SAME gRPC ClientConn.
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, origin+"/disconnect", nil)
	if err != nil {
		return checks, err
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return checks, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return checks, fmt.Errorf("disconnect: %s", resp.Status)
	}
	if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}, grpc.WaitForReady(true)); err != nil {
		return checks, fmt.Errorf("reconnect: %w", err)
	}
	checks = append(checks, "gRPC reconnect opens a fresh WebSocket")

	wrongOrg := f.Certificate
	wrongOrg.OrganizationID++
	bad, err := connect(wrongOrg)
	if err != nil {
		return checks, err
	}
	badCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = bad.AgentService.GetAgentVersion(badCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	bad.Close()
	if err == nil {
		return checks, fmt.Errorf("wrong organization accepted")
	}
	if !strings.Contains(err.Error(), "server certificate belongs to org 7, expected org 8") {
		return checks, fmt.Errorf("wrong organization failed for an unexpected reason: %w", err)
	}
	checks = append(checks, "Wendy verifier rejects the wrong organization: "+err.Error())

	// Override only the negative case to omit the client certificate while
	// still verifying the fixture server with the experiment's CA.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.Certificate.PemCertificateChain)) {
		return checks, fmt.Errorf("invalid fixture CA")
	}
	noCert, err := connect(f.Certificate, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, ServerName: "wasm-agent.test", RootCAs: roots,
	})))
	if err != nil {
		return checks, err
	}
	badCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	_, err = noCert.AgentService.GetAgentVersion(badCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	noCert.Close()
	if err == nil {
		return checks, fmt.Errorf("missing client certificate accepted")
	}
	if !strings.Contains(err.Error(), "certificate required") {
		return checks, fmt.Errorf("missing certificate failed for an unexpected reason: %w", err)
	}
	checks = append(checks, "server rejects a missing client certificate: "+err.Error())
	return checks, nil
}
