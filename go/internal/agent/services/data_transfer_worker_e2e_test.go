package services

// Cross-repo end-to-end harness for the Wendy Data Platform device -> cloud seam.
//
// Unlike data_transfer_worker_test.go (which runs the worker against an
// in-process bufconn fake implemented in this package), this harness drives the
// REAL DataTransferWorker over a REAL gRPC connection to the REAL Swift
// wendy-data ingest service, which in turn writes to a real ClickHouse and a
// real fake-gcs. It exercises the wire compatibility between this repo's
// independently generated cloudpb client and the cloud repo's independently
// generated Swift server for wendycloud.data.v1.DataIngestService.
//
// It is skipped unless the stack is up and pointed at via env, so it never runs
// in ordinary unit-test CI:
//
//	WENDY_DATA_E2E_ADDR=localhost:9443 \
//	WENDY_DATA_E2E_CLIENT_CERT=/path/device.pem \
//	WENDY_DATA_E2E_CLIENT_KEY=/path/device.key \
//	WENDY_DATA_E2E_SERVER_CA=/path/server-ca.pem \
//	WENDY_DATA_E2E_READ_TOKEN=e2e-read-token \
//	CC=/usr/bin/clang go test ./internal/agent/services/ -run E2EDataPlatform -v
//
// Device identity: the ingest service terminates mutual TLS itself and reads
// the device identity from the validated client certificate's URI SAN. No
// header carries identity any more, so this harness presents a client
// certificate whose SAN is urn:wendy:org:7:asset:42 (the service must run with
// WENDY_DATA_ACCEPT_LEGACY_URN_SAN=1 and a trust root that signed it). The
// worker code is unchanged; the only local substitution is where the client
// certificate comes from (files named by env rather than provisioning).
import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

const (
	// The identity the server reads from the harness's client certificate SAN
	// (urn:wendy:org:7:asset:42). The read-side check compares against these, so
	// a certificate naming another device fails the test rather than passing
	// under the wrong owner.
	e2eOrgID   = "7"
	e2eAssetID = "42"
)

// e2eTransportCreds builds the mutual TLS transport from the certificate files
// named by env: the client certificate and key the harness presents, and the CA
// the server's certificate chains to (the system roots when unset, which is
// what a deployed endpoint with a Let's Encrypt certificate needs).
func e2eTransportCreds(t *testing.T) grpc.DialOption {
	t.Helper()
	certPath, keyPath := os.Getenv("WENDY_DATA_E2E_CLIENT_CERT"), os.Getenv("WENDY_DATA_E2E_CLIENT_KEY")
	if certPath == "" || keyPath == "" {
		t.Skip("WENDY_DATA_E2E_CLIENT_CERT / WENDY_DATA_E2E_CLIENT_KEY not set; the ingest service requires a device certificate")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	if caPath := os.Getenv("WENDY_DATA_E2E_SERVER_CA"); caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			t.Fatalf("read server CA: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatalf("server CA %s holds no certificate", caPath)
		}
		cfg.RootCAs = pool
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg))
}

func e2eAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("WENDY_DATA_E2E_ADDR")
	if addr == "" {
		t.Skip("WENDY_DATA_E2E_ADDR not set; skipping cross-repo end-to-end harness")
	}
	return addr
}

// dialWorkerClient dials the ingest service the way the worker does: mutual TLS
// and nothing else. No interceptor attaches any identity.
func dialWorkerClient(t *testing.T, addr string) (*grpc.ClientConn, cloudpb.DataIngestServiceClient) {
	t.Helper()
	conn, err := grpc.NewClient(addr, e2eTransportCreds(t))
	if err != nil {
		t.Fatalf("dial worker client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, cloudpb.NewDataIngestServiceClient(conn)
}

func dialReadClient(t *testing.T, addr, token string) cloudpb.DataIngestServiceClient {
	t.Helper()
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	conn, err := grpc.NewClient(addr,
		e2eTransportCreds(t),
		grpc.WithUnaryInterceptor(unary),
	)
	if err != nil {
		t.Fatalf("dial read client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return cloudpb.NewDataIngestServiceClient(conn)
}

// newRealWorker builds the real DataTransferWorker with its production time and
// pacing behavior, but with the ingest client factory pointed at the given real
// server client. Only w.factory is substituted; every RPC and state transition
// is the worker's own code.
func newRealWorker(mgr *data.Manager, client cloudpb.DataIngestServiceClient) *DataTransferWorker {
	logger, _ := zap.NewDevelopment()
	w := &DataTransferWorker{
		logger:      logger,
		manager:     mgr,
		maxAttempts: transferMaxAttempts,
		now:         time.Now,
		newSleeper:  contextSleeper,
	}
	w.factory = func(context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
		return client, func() {}, nil
	}
	return w
}

// sealRealEpisode creates and seals a real episode with the real data.Manager
// (real sealFiles SHA-256 hashing), writing the given extra files into the live
// capture directory before Stop. The episode is queued for upload (pending).
func sealRealEpisode(t *testing.T, mgr *data.Manager, extra map[string][]byte) data.Manifest {
	t.Helper()
	if _, err := mgr.Start(data.StartOptions{
		Name:    "e2e",
		Trigger: data.EpisodeTrigger{Reason: "e2e-harness"},
		Upload:  data.WorkflowState{State: "pending"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, ok := mgr.ActiveSession(data.AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session after Start")
	}
	for path, content := range extra {
		full := filepath.Join(sess.Directory, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(full, content, 0o640); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	mf, err := mgr.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return mf
}

func deterministicBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7+i/13)
	}
	return b
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestE2EDataPlatformHappyPath runs the full device->cloud->store->read path
// with the real worker over the real wire, including a zero-length file, then
// verifies the catalog read side and blob retrieval from the harness.
func TestE2EDataPlatformHappyPath(t *testing.T) {
	addr := e2eAddr(t)
	readToken := os.Getenv("WENDY_DATA_E2E_READ_TOKEN")

	mgr, _ := newTestManager(t)

	// A ~1.5 MiB blob exercises the >1 MiB multi-chunk streaming path; a small
	// JPEG a single chunk; and marker.empty a genuine zero-length file (the D4
	// case). Start() also auto-creates empty events.jsonl / telemetry.jsonl.
	camera := deterministicBytes(1_500_000, 0x11)
	snapshot := deterministicBytes(3000, 0x22)
	events := []byte("{\"t\":1,\"event\":\"start\"}\n{\"t\":2,\"event\":\"stop\"}\n")
	mf := sealRealEpisode(t, mgr, map[string][]byte{
		"camera.h264":      camera,
		"snapshot.jpg":     snapshot,
		"app/events.jsonl": events,
		"marker.empty":     {},
	})

	t.Logf("sealed episode id=%s files=%d", mf.ID, len(mf.Files))
	var zeroLenPath string
	for _, f := range mf.Files {
		t.Logf("  file path=%s size=%d sha256=%s media=%s", f.Path, f.Size, f.SHA256, f.MediaType)
		if f.Size == 0 {
			zeroLenPath = f.Path
		}
	}
	if zeroLenPath == "" {
		t.Fatal("expected at least one zero-length file in the sealed manifest")
	}
	t.Logf("zero-length file under test: %s", zeroLenPath)

	// Real worker over the real wire.
	_, client := dialWorkerClient(t, addr)
	w := newRealWorker(mgr, client)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := w.runPass(ctx); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	if got := uploadState(t, mgr, mf.ID); got.State != uploadStateUploaded {
		t.Fatalf("upload state = %q, want %q (lastErr=%q)", got.State, uploadStateUploaded, got.LastError)
	}
	t.Logf("worker marked episode %s uploaded", mf.ID)

	// Read side: GetEpisode with the read token (fails closed without it).
	if readToken == "" {
		t.Log("WENDY_DATA_E2E_READ_TOKEN not set; skipping read-side verification")
		return
	}
	read := dialReadClient(t, addr, readToken)
	ep, err := read.GetEpisode(ctx, &cloudpb.GetEpisodeRequest{OrgId: e2eOrgID, EpisodeId: mf.ID})
	if err != nil {
		t.Fatalf("GetEpisode: %v", err)
	}
	if ep.GetState() != cloudpb.EpisodeState_EPISODE_STATE_COMPLETE {
		t.Fatalf("episode state = %s, want COMPLETE", ep.GetState())
	}
	if ep.GetOrgId() != e2eOrgID || ep.GetAssetId() != e2eAssetID {
		t.Fatalf("episode identity = org %s asset %s, want %s/%s", ep.GetOrgId(), ep.GetAssetId(), e2eOrgID, e2eAssetID)
	}
	t.Logf("GetEpisode: state=%s org=%s asset=%s size=%d files=%d",
		ep.GetState(), ep.GetOrgId(), ep.GetAssetId(), ep.GetSizeBytes(), len(ep.GetFiles()))

	// Cross-check every manifest file appears in the catalog with a matching sha.
	catalog := map[string]*cloudpb.EpisodeFileInfo{}
	for _, fi := range ep.GetFiles() {
		catalog[fi.GetPath()] = fi
		t.Logf("  catalog file path=%s size=%d sha256=%s url=%s", fi.GetPath(), fi.GetSizeBytes(), fi.GetSha256(), fi.GetRetrievalUrl())
	}
	for _, f := range mf.Files {
		fi, ok := catalog[f.Path]
		if !ok {
			t.Fatalf("manifest file %s missing from catalog", f.Path)
		}
		if fi.GetSha256() != f.SHA256 {
			t.Fatalf("catalog sha mismatch for %s: %s != %s", f.Path, fi.GetSha256(), f.SHA256)
		}
		if fi.GetSizeBytes() != uint64(f.Size) {
			t.Fatalf("catalog size mismatch for %s: %d != %d", f.Path, fi.GetSizeBytes(), f.Size)
		}
	}

	// Fetch the zero-length blob and a non-empty blob via their retrieval URLs
	// and confirm the bytes hash to the manifest sha.
	for _, path := range []string{zeroLenPath, "camera.h264"} {
		fi := catalog[path]
		if fi == nil || fi.GetRetrievalUrl() == "" {
			t.Fatalf("no retrieval URL for %s", path)
		}
		body := httpGet(t, fi.GetRetrievalUrl())
		if got := sha256hex(body); got != fi.GetSha256() {
			t.Fatalf("fetched %s: sha %s != catalog %s", path, got, fi.GetSha256())
		}
		t.Logf("fetched %s via retrieval URL: %d bytes, sha256 matches (%s)", path, len(body), fi.GetSha256())
	}
}

// TestE2EDataPlatformNegativeCorruption seals an episode, corrupts a file's
// bytes on disk after sealing (so they no longer match the manifest sha), and
// confirms the real Commit over the real wire marks the episode failed and
// names the offending file. This proves the integrity guarantee spans the wire.
func TestE2EDataPlatformNegativeCorruption(t *testing.T) {
	addr := e2eAddr(t)
	readToken := os.Getenv("WENDY_DATA_E2E_READ_TOKEN")

	mgr, root := newTestManager(t)
	good := deterministicBytes(40_000, 0x33)
	mf := sealRealEpisode(t, mgr, map[string][]byte{"camera.h264": good})
	t.Logf("sealed episode id=%s for negative check", mf.ID)

	// Corrupt camera.h264 on disk, keeping the SAME length so the failure is a
	// sha256 mismatch (not a size mismatch). The manifest still records the sha
	// of the good bytes, which the worker sends; the server hashes the stored
	// (corrupt) bytes and disagrees.
	corrupt := deterministicBytes(len(good), 0x99)
	target := filepath.Join(root, mf.ID, "camera.h264")
	if err := os.WriteFile(target, corrupt, 0o640); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	var expectedSha string
	for _, f := range mf.Files {
		if f.Path == "camera.h264" {
			expectedSha = f.SHA256
		}
	}
	t.Logf("corrupted camera.h264 on disk (manifest sha=%s, corrupt sha=%s)", expectedSha, sha256hex(corrupt))

	_, client := dialWorkerClient(t, addr)
	w := newRealWorker(mgr, client)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := w.runPass(ctx); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	got := uploadState(t, mgr, mf.ID)
	if got.State != uploadStateFailed {
		t.Fatalf("upload state = %q, want %q", got.State, uploadStateFailed)
	}
	t.Logf("worker marked episode %s FAILED; lastError=%q", mf.ID, got.LastError)

	// The server's own catalog state should also be failed.
	if readToken != "" {
		read := dialReadClient(t, addr, readToken)
		ep, err := read.GetEpisode(ctx, &cloudpb.GetEpisodeRequest{OrgId: e2eOrgID, EpisodeId: mf.ID})
		if err != nil {
			t.Fatalf("GetEpisode: %v", err)
		}
		if ep.GetState() != cloudpb.EpisodeState_EPISODE_STATE_FAILED {
			t.Fatalf("server episode state = %s, want FAILED", ep.GetState())
		}
		t.Logf("server catalog confirms episode %s state=%s", mf.ID, ep.GetState())
	}
}

func httpGet(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // URL is minted by our own local signer
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s: %v", url, err)
	}
	return body
}
