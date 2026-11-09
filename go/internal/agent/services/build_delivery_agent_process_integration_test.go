//go:build integration

package services_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Run with an arm64 Linux agent binary and an image containing containerd/dbus:
// WENDY_DELIVERY_AGENT_BINARY=/absolute/path/wendy-agent \
// WENDY_DELIVERY_DOCKER_IMAGE=wendy-delivery-lab \
// go test -tags=integration ./go/internal/agent/services -run TestBuildDeliveryAgentProcessOfflineRecovery -v
// Build the lab image with testdata/delivery-agent/Dockerfile. Runtime networking
// is an internal Docker network: neither admission nor delivery can reach Cloud.
func TestBuildDeliveryAgentProcessOfflineRecovery(t *testing.T) {
	binary := os.Getenv("WENDY_DELIVERY_AGENT_BINARY")
	if binary == "" {
		t.Skip("set WENDY_DELIVERY_AGENT_BINARY to run the complete agent in isolated Docker")
	}
	image := os.Getenv("WENDY_DELIVERY_DOCKER_IMAGE")
	if image == "" {
		t.Fatal("set WENDY_DELIVERY_DOCKER_IMAGE; build testdata/delivery-agent/Dockerfile")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	f := deliverypki.New(t)
	stateDir := t.TempDir()
	state := map[string]any{"enrolled": true, "cloudHost": "https://cloud.invalid", "certPem": f.Target.PEM, "chainPem": f.Chain, "principalURI": deliverypki.TargetPrincipal}
	data, _ := json.Marshal(state)
	if err = os.WriteFile(filepath.Join(stateDir, "provisioning.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stateDir, "device-key.pem"), []byte(f.Target.Key), 0600); err != nil {
		t.Fatal(err)
	}
	name := "wdy-delivery-" + uuid.NewString()[:8]
	network := name + "-offline"
	docker := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	docker("network", "create", "--internal", network)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	if strings.TrimSpace(string(docker("network", "inspect", "--format", "{{.Internal}}", network))) != "true" {
		t.Fatal("test network permits external routing")
	}
	for _, volume := range []string{name + "-containerd", name + "-agent"} {
		docker("volume", "create", volume)
		t.Cleanup(func() { exec.Command("docker", "volume", "rm", volume).Run() })
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "60", name).CombinedOutput()
			t.Logf("agent logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "-f", name).Run()
	})
	script := `mkdir -p /run/dbus /run/containerd
rm -f /run/dbus/pid
dbus-daemon --system --fork
containerd >/tmp/containerd.log 2>&1 &
for attempt in $(seq 1 100); do ctr version >/dev/null 2>&1 && break; sleep 0.1; done
exec /audit/agent`
	docker("run", "-d", "--name", name, "--platform", "linux/arm64", "--privileged", "--network", network, "--tmpfs", "/run", "--tmpfs", "/tmp", "--mount", "type=bind,src="+binary+",dst=/audit/agent,readonly", "--mount", "type=bind,src="+stateDir+",dst=/audit/state", "--mount", "type=volume,src="+name+"-containerd,dst=/var/lib/containerd,volume-nocopy", "--mount", "type=volume,src="+name+"-agent,dst=/var/lib/wendy,volume-nocopy", "-e", "WENDY_CONFIG_PATH=/audit/state", "-e", "WENDY_CONTAINERD_ADDR=/run/containerd/containerd.sock", "--entrypoint", "sh", image, "-c", script)
	// Docker's isolated bridge mode intentionally suppresses published ports.
	// A separate transparent ingress relay exposes only the two TLS endpoints;
	// the agent itself retains no external network or default Internet route.
	relayBinary := filepath.Join(t.TempDir(), "relay")
	compile := exec.Command("go", "build", "-o", relayBinary, "./testdata/delivery-agent/relay")
	compile.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile test relay: %v %s", err, output)
	}
	targetIP := strings.TrimSpace(string(docker("inspect", "--format", fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", network), name)))
	ingress := name + "-ingress"
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", ingress).Run() })
	docker("run", "-d", "--name", ingress, "--platform", "linux/arm64", "--network", "bridge", "--cap-drop", "ALL", "--read-only", "--user", "65534", "--mount", "type=bind,src="+relayBinary+",dst=/audit/relay,readonly", "-p", "127.0.0.1::50052", "-p", "127.0.0.1::5000", "--entrypoint", "/audit/relay", image, "-target", targetIP)
	docker("network", "connect", network, ingress)
	address := func(port string) string {
		binding := strings.TrimSpace(string(docker("port", ingress, port+"/tcp")))
		return strings.Split(binding, "\n")[0]
	}
	agentAddress, registryAddress := address("50052"), address("5000")
	connect := func(c deliverypki.Credential) *grpc.ClientConn {
		conn, err := grpc.NewClient(agentAddress, grpc.WithTransportCredentials(credentials.NewTLS(f.ClientTLS(t, c))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	operator := f.Mint(t, deliverypki.OperatorPrincipal, nil)
	opConn := connect(operator)
	opDelivery := agentpbv2.NewWendyBuildDeliveryServiceClient(opConn)
	waitReady := func(conn *grpc.ClientConn) {
		t.Helper()
		deadline := time.Now().Add(75 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			cap, err := agentpbv2.NewWendyBuildDeliveryServiceClient(conn).GetDeliveryCapabilities(ctx, &agentpbv2.GetDeliveryCapabilitiesRequest{})
			cancel()
			if err == nil && cap.GetContractVersion() == 1 {
				if _, err = builddelivery.VerifyBuilderCertificate(cap.GetCertificateChain(), []*x509.Certificate{f.Root}, f.TargetID, time.Now()); err != nil {
					t.Fatal(err)
				}
				return
			}
			if status.Code(err) == codes.Unimplemented {
				t.Fatal("test binary does not contain delivery admission")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("complete target agent did not become ready")
	}
	waitReady(opConn)
	t.Log("complete agent and containerd ready on an internal network")
	builder := connect(f.Builder)
	containerClient := agentpb.NewWendyContainerServiceClient(builder)
	deliveryClient := agentpbv2.NewWendyBuildDeliveryServiceClient(builder)
	tlsTransport := &http.Transport{TLSClientConfig: f.ClientTLS(t, f.Builder)}
	defer tlsTransport.CloseIdleConnections()
	httpClient := &http.Client{Transport: tlsTransport, Timeout: 15 * time.Second}
	registryURL := "https://" + registryAddress
	request := func(method, path, session string, body []byte) *http.Response {
		t.Helper()
		base, _ := url.Parse(registryURL)
		relative, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, base.ResolveReference(relative).String(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(builddelivery.HTTPHeader, session)
		req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	want := func(response *http.Response, code int) {
		t.Helper()
		if response.StatusCode != code {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("registry HTTP=%d want=%d: %s", response.StatusCode, code, data)
		}
	}
	// A small real OCI layer is large enough to exercise several chunks.
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	contents := bytes.Repeat([]byte("offline authorized image\n"), 32000)
	if err = tw.WriteHeader(&tar.Header{Name: "proof.txt", Mode: 0644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	layerBytes := layer.Bytes()
	layerDigest := digest.FromBytes(layerBytes)
	configBytes, err := json.Marshal(ocispec.Image{Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"}, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layerDigest}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: digest.FromBytes(configBytes), Size: int64(len(configBytes))}, Layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: layerDigest, Size: int64(len(layerBytes))}}}
	manifest.SchemaVersion = 2
	manifestBytes, _ := json.Marshal(manifest)
	output := digest.FromBytes(manifestBytes).String()
	// The signing certificate expires before either job finishes. The ordinary
	// admission connection has a distinct primary operator key and is closed.
	now := time.Now().Truncate(time.Second)
	f.Operator = f.MintUntil(t, deliverypki.OperatorPrincipal, []string{"entitlement:deployment:create:allow"}, now.Add(8*time.Second))
	admit := func(reference string) string {
		c := f.Claims(reference)
		c.IssuedAt = now.Unix()
		c.AdmitBefore = now.Add(5 * time.Second).Unix()
		c.ValidUntil = now.Add(5 * time.Minute).Unix()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		reply, err := opDelivery.AdmitDelivery(ctx, &agentpbv2.AdmitDeliveryRequest{AuthorizationJws: f.Sign(t, c)})
		if err != nil {
			t.Fatal(err)
		}
		return reply.GetSession()
	}
	chunkSession, registrySession := admit("offline-chunk:latest"), admit("offline-registry:latest")
	chunkContext := func() context.Context {
		return metadata.AppendToOutgoingContext(context.Background(), builddelivery.MetadataKey, chunkSession)
	}
	if _, err = containerClient.QueryChunks(context.Background(), &agentpb.QueryChunksRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("device staged without operator grant: %v", err)
	}
	refs, err := chunk.ChunkBytes(layerBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 2 {
		t.Fatal("layer did not exercise multiple chunks")
	}
	uploadChunks := func(refs []chunk.Ref) {
		stream, err := containerClient.WriteChunks(chunkContext())
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range refs {
			if err = stream.Send(&agentpb.WriteChunksRequest{Hash: ref.Hash[:], Data: layerBytes[ref.Offset : ref.Offset+ref.Len]}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = stream.CloseAndRecv(); err != nil {
			t.Fatal(err)
		}
	}
	uploadChunks(refs[:1])
	upload := request(http.MethodPost, "/v2/offline-registry/blobs/uploads/", registrySession, nil)
	want(upload, http.StatusAccepted)
	uploadLocation := upload.Header.Get("Location")
	split := len(layerBytes) / 2
	want(request(http.MethodPatch, uploadLocation, registrySession, layerBytes[:split]), http.StatusAccepted)
	opConn.Close()
	t.Log("operator disconnected; partial chunk and OCI uploads staged")
	if delay := time.Until(f.Operator.Leaf.NotAfter.Add(100 * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	docker("restart", "--time", "3", name)
	builder.Close()
	tlsTransport.CloseIdleConnections()
	builder = connect(f.Builder)
	containerClient = agentpb.NewWendyContainerServiceClient(builder)
	deliveryClient = agentpbv2.NewWendyBuildDeliveryServiceClient(builder)
	waitReady(builder)
	t.Log("agent restarted after signing certificate expiry; resuming without an operator connection")
	hashes := make([][]byte, len(refs))
	for i, ref := range refs {
		hashes[i] = bytes.Clone(ref.Hash[:])
	}
	missing, err := containerClient.QueryChunks(chunkContext(), &agentpb.QueryChunksRequest{ChunkHashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	// Staging is intentionally a process-scoped cache: startup maintenance may
	// discard it. Resumption re-queries actual availability and sends the misses;
	// the durable authority, output binding and byte budget survive independently.
	missingSet := map[string]bool{}
	for _, hash := range missing.GetMissingHashes() {
		missingSet[string(hash)] = true
	}
	var resend []chunk.Ref
	for _, ref := range refs {
		if missingSet[string(ref.Hash[:])] {
			resend = append(resend, ref)
			delete(missingSet, string(ref.Hash[:]))
		}
	}
	uploadChunks(resend)
	ctx, cancel := context.WithTimeout(chunkContext(), 30*time.Second)
	defer cancel()
	_, err = containerClient.PrepareImage(ctx, &agentpb.RunContainerLayersRequest{ImageName: "localhost:5000/offline-chunk:latest", ImageConfig: configBytes, Layers: []*agentpb.RunContainerLayerHeader{{Digest: layerDigest.String(), DiffId: layerDigest.String(), Size: int64(len(layerBytes)), Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE, ChunkHashes: hashes}}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uploadLocation)
	q := u.Query()
	q.Set("digest", layerDigest.String())
	u.RawQuery = q.Encode()
	want(request(http.MethodPut, u.String(), registrySession, layerBytes[split:]), http.StatusCreated)
	cfgUpload := request(http.MethodPost, "/v2/offline-registry/blobs/uploads/?digest="+digest.FromBytes(configBytes).String(), registrySession, configBytes)
	want(cfgUpload, http.StatusCreated)
	want(request(http.MethodPut, "/v2/offline-registry/manifests/latest", registrySession, manifestBytes), http.StatusCreated)
	for _, session := range []string{chunkSession, registrySession} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		state, err := deliveryClient.GetDeliveryStatus(ctx, &agentpbv2.GetDeliveryStatusRequest{Session: session})
		cancel()
		if err != nil || !state.GetCompleted() || state.GetOutputDigest() != output {
			t.Fatalf("completed output mismatch: %v %v", state, err)
		}
	}
	images := string(docker("exec", name, "ctr", "-n", "default", "images", "ls"))
	for _, ref := range []string{"localhost:5000/offline-chunk:latest", "localhost:5000/offline-registry:latest"} {
		if !strings.Contains(images, ref) {
			t.Fatalf("real containerd image missing %s: %s", ref, images)
		}
	}
	want(request(http.MethodGet, "/v2/offline-registry/blobs/"+layerDigest.String(), registrySession, nil), http.StatusForbidden)
	if _, err = containerClient.StopContainer(chunkContext(), &agentpb.StopContainerRequest{AppName: "anything"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("image grant elevated to app control: %v", err)
	}
	t.Log("both transports published the same OCI manifest in real containerd after offline restart and operator-certificate expiry")
}
