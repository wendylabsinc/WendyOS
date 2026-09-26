//go:build !js

// Local-only fixture: ephemeral test credentials, a mock Wendy service, and a
// WebSocket-to-TCP relay. This is deliberately not a production relay server.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracespb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type service struct {
	agentpb.UnimplementedWendyAgentServiceServer
	agentpb.UnimplementedWendyShellServiceServer
	agentpb.UnimplementedWendyTelemetryServiceServer
}

func (service) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	t, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(t.State.PeerCertificates) == 0 || t.State.PeerCertificates[0].Subject.CommonName != "wasm-operator" {
		return nil, status.Error(codes.Unauthenticated, "no verified operator certificate")
	}
	return &agentpb.GetAgentVersionResponse{Version: "wasm-mtls-fixture"}, nil
}

// HostShell echoes bytes using the real Wendy protobuf stream. No shell runs.
func (service) HostShell(stream grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetStart() == nil {
		return status.Error(codes.InvalidArgument, "expected Start")
	}
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: req.GetStdinData()}}); err != nil {
			return err
		}
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	port := flag.Int("port", 8787, "loopback HTTP port; 0 selects an available port")
	wasm := flag.String("wasm", "/tmp/wendy-wasm-grpc.wasm", "compiled browser client")
	mldsaKeys := flag.Bool("mldsa", false, "use ML-DSA-65 certificates, matching browser sign-in")
	mismatchCA := flag.Bool("mismatched-ca-hint", false, "advertise an unrelated issuer while verifying the real chain")
	flag.Parse()
	serverTLS, operator, err := certificates(*mldsaKeys, *mismatchCA)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	var handshakes atomic.Int64
	serverTLS.VerifyConnection = func(tls.ConnectionState) error {
		handshakes.Add(1)
		return nil
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentpb.RegisterWendyAgentServiceServer(server, service{})
	agentpb.RegisterWendyTelemetryServiceServer(server, service{})
	agentpb.RegisterWendyShellServiceServer(server, service{})
	defer server.Stop()
	go func() {
		if err := server.Serve(listener); err != nil {
			log.Printf("gRPC: %v", err)
		}
	}()

	httpListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	defer httpListener.Close()
	origin := "http://" + httpListener.Addr().String()
	var mu sync.Mutex
	connections := map[*websocket.Conn]bool{}
	var opened int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /connection-stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"opened": opened, "active": len(connections), "handshakes": handshakes.Load()})
	})
	mux.HandleFunc("GET /tunnel", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		mu.Lock()
		connections[ws] = true
		opened++
		mu.Unlock()
		defer func() { mu.Lock(); delete(connections, ws); mu.Unlock() }()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		// Fixed target. The browser cannot turn this into an arbitrary TCP proxy.
		tcp, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		if err != nil {
			return
		}
		defer tcp.Close()
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		done := make(chan struct{})
		go func() { _, _ = io.Copy(tcp, conn); _ = tcp.Close(); close(done) }()
		_, _ = io.Copy(conn, tcp)
		cancel()
		_ = ws.CloseNow()
		<-done
	})
	mux.HandleFunc("POST /disconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin {
			http.Error(w, "origin mismatch", http.StatusForbidden)
			return
		}
		mu.Lock()
		for ws := range connections {
			_ = ws.CloseNow()
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /fixture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"certificate": operator})
	})
	mux.HandleFunc("GET /client.wasm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/wasm")
		http.ServeFile(w, r, *wasm)
	})
	mux.HandleFunc("GET /wasm_exec.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(runtime.GOROOT(), "lib", "wasm", "wasm_exec.js"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	})
	// Block DNS rebinding to the fixture credential endpoint.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != httpListener.Addr().String() {
			http.Error(w, "host mismatch", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	fmt.Printf("WASM experiment: %s\n", origin)
	return (&http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}).Serve(httpListener)
}

func certificates(useMLDSA, mismatchCA bool) (*tls.Config, config.CertificateInfo, error) {
	var info config.CertificateInfo
	generate := func() (crypto.Signer, error) {
		if useMLDSA {
			return mldsa.GenerateKey(mldsa.MLDSA65())
		}
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	key, err := generate()
	if err != nil {
		return nil, info, err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "WASM experiment CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		return nil, info, err
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, info, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	issue := func(serial int64, name, identity string, usage x509.ExtKeyUsage) ([]byte, []byte, error) {
		leafKey, err := generate()
		if err != nil {
			return nil, nil, err
		}
		uri, err := url.Parse(identity)
		if err != nil {
			return nil, nil, err
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{name}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), key)
		if err != nil {
			return nil, nil, err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
	}
	serverCert, serverKey, err := issue(2, "wasm-agent.test", "urn:wendy:org:7:asset:42", x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, info, err
	}
	clientCert, clientKey, err := issue(3, "wasm-operator", "urn:wendy:org:7:user:wasm", x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, info, err
	}
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, info, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	info = config.CertificateInfo{PemCertificate: string(clientCert), PemPrivateKey: string(clientKey), PemCertificateChain: string(caPEM), OrganizationID: 7}
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	if mismatchCA {
		hint := *ca
		hint.RawSubject = []byte("a different issuing authority")
		hints := x509.NewCertPool()
		hints.AddCert(&hint)
		serverTLS.ClientCAs = hints
		serverTLS.ClientAuth = tls.RequireAnyClientCert
		serverTLS.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return fmt.Errorf("operator certificate missing")
			}
			chain := make([]*x509.Certificate, 0, len(raw))
			for _, der := range raw {
				c, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				chain = append(chain, c)
			}
			return certs.VerifyPeerCertificateChain(chain[0], chain[1:], []*x509.Certificate{ca}, x509.ExtKeyUsageClientAuth, time.Now(), time.Now())
		}
	}
	return serverTLS, info, nil
}

const page = `<!doctype html>
<meta charset="utf-8"><title>Wendy WASM mTLS experiment</title>
<h1>Wendy WASM mTLS experiment</h1>
<p>Local fixture. Uses temporary test certificates and an echo service, not a device shell.</p>
<pre id="result">Running browser WASM checks...</pre>
<script src="/wasm_exec.js"></script>
<script>
(async () => {
  try {
    const go = new Go();
    const {instance} = await WebAssembly.instantiateStreaming(fetch('/client.wasm'), go.importObject);
    await go.run(instance);
  } catch (error) {
    window.wasmResult = {ok: false, error: String(error)};
    document.querySelector('#result').textContent = JSON.stringify(window.wasmResult);
  }
})();
</script>`

func (service) StreamLogs(_ *agentpb.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpb.StreamLogsResponse]) error {
	if err := stream.Send(&agentpb.StreamLogsResponse{Logs: &logspb.ExportLogsServiceRequest{}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func (service) StreamMetrics(_ *agentpb.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpb.StreamMetricsResponse]) error {
	if err := stream.Send(&agentpb.StreamMetricsResponse{Metrics: &metricspb.ExportMetricsServiceRequest{}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
func (service) StreamTraces(_ *agentpb.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpb.StreamTracesResponse]) error {
	if err := stream.Send(&agentpb.StreamTracesResponse{Traces: &tracespb.ExportTraceServiceRequest{}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}
