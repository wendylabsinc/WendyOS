package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---------- mock containerd client ----------

type mockContainerdClient struct {
	containers     []*agentpb.AppContainer
	listErr        error
	stopErr        error
	deleteErr      error
	layers         []*agentpb.LayerHeader
	listLayersErr  error
	writeLayerErr  error
	writtenDigest  string
	writtenData    []byte
	createErr      error
	progressPhases []agentpb.CreateContainerProgress_Phase
	startOutputCh  chan ContainerOutput
	startErr       error
}

func (m *mockContainerdClient) ListContainers(_ context.Context) ([]*agentpb.AppContainer, error) {
	return m.containers, m.listErr
}
func (m *mockContainerdClient) StopContainer(_ context.Context, _ string) error {
	return m.stopErr
}
func (m *mockContainerdClient) DeleteContainer(_ context.Context, _ string, _ bool) error {
	return m.deleteErr
}
func (m *mockContainerdClient) ListLayers(_ context.Context) ([]*agentpb.LayerHeader, error) {
	return m.layers, m.listLayersErr
}
func (m *mockContainerdClient) WriteLayer(_ context.Context, digest string, reader io.Reader, _ int64) error {
	m.writtenDigest = digest
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.writtenData = data
	return m.writeLayerErr
}
func (m *mockContainerdClient) AssembleImage(_ context.Context, _ string, _ []*agentpb.RunContainerLayerHeader) error {
	return nil
}
func (m *mockContainerdClient) CreateContainer(_ context.Context, _ *agentpb.CreateContainerRequest, _ *appconfig.AppConfig) error {
	return m.createErr
}
func (m *mockContainerdClient) CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress ProgressFunc) error {
	if onProgress != nil {
		for _, phase := range m.progressPhases {
			onProgress(&agentpb.CreateContainerProgress{Phase: phase})
		}
	}
	return m.CreateContainer(ctx, req, appCfg)
}
func (m *mockContainerdClient) StartContainer(_ context.Context, _ string) (<-chan ContainerOutput, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.startOutputCh, nil
}

func (m *mockContainerdClient) StartContainerWithStdin(_ context.Context, _ string, _ io.Reader) (<-chan ContainerOutput, error) {
	if m.startErr != nil {
		return nil, m.startErr
	}
	return m.startOutputCh, nil
}

// attachTestMock embeds mockContainerdClient and overrides StartContainerWithStdin
// so tests can capture the appName and stdin reader passed by AttachContainer.
type attachTestMock struct {
	mockContainerdClient
	onStartWithStdin func(appName string, stdin io.Reader) (<-chan ContainerOutput, error)
}

func (m *attachTestMock) StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader) (<-chan ContainerOutput, error) {
	if m.onStartWithStdin != nil {
		return m.onStartWithStdin(appName, stdin)
	}
	return m.mockContainerdClient.StartContainerWithStdin(ctx, appName, stdin)
}

// ---------- bufconn helper ----------

func startContainerServer(t *testing.T, client ContainerdClient, opts ...ContainerServiceOption) (agentpb.WendyContainerServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	logger := zap.NewNop()
	svc := NewContainerService(logger, client, opts...)
	agentpb.RegisterWendyContainerServiceServer(srv, svc)

	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	cl := agentpb.NewWendyContainerServiceClient(conn)
	cleanup := func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}
	return cl, cleanup
}

// ---------- tests ----------

func TestListContainers(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{AppName: "app-one", AppVersion: "1.0"},
		{AppName: "app-two", AppVersion: "2.0"},
	}
	client, cleanup := startContainerServer(t, &mockContainerdClient{containers: containers})
	defer cleanup()

	stream, err := client.ListContainers(context.Background(), &agentpb.ListContainersRequest{})
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}

	var received []*agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		received = append(received, resp.Container)
	}

	if len(received) != 2 {
		t.Fatalf("len(containers) = %d; want 2", len(received))
	}
	if received[0].AppName != "app-one" {
		t.Errorf("containers[0].AppName = %q; want app-one", received[0].AppName)
	}
}

func TestStopContainer(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.StopContainer(context.Background(), &agentpb.StopContainerRequest{
		AppName: "test-app",
	})
	if err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
}

func TestDeleteContainer(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:     "test-app",
		DeleteImage: false,
	})
	if err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}
}

func TestDeleteContainer_WithImage(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName:     "test-app",
		DeleteImage: true,
	})
	if err != nil {
		t.Fatalf("DeleteContainer with image: %v", err)
	}
}

func TestDeleteContainer_Error(t *testing.T) {
	client, cleanup := startContainerServer(t, &mockContainerdClient{
		deleteErr: fmt.Errorf("container not found"),
	})
	defer cleanup()

	_, err := client.DeleteContainer(context.Background(), &agentpb.DeleteContainerRequest{
		AppName: "missing-app",
	})
	if err == nil {
		t.Fatal("expected error from DeleteContainer")
	}
}

func TestListLayers(t *testing.T) {
	layers := []*agentpb.LayerHeader{
		{Digest: "sha256:abc123", Size: 1024},
		{Digest: "sha256:def456", Size: 2048},
	}
	client, cleanup := startContainerServer(t, &mockContainerdClient{layers: layers})
	defer cleanup()

	stream, err := client.ListLayers(context.Background(), &agentpb.ListLayersRequest{})
	if err != nil {
		t.Fatalf("ListLayers: %v", err)
	}

	var received []*agentpb.LayerHeader
	for {
		layer, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		received = append(received, layer)
	}

	if len(received) != 2 {
		t.Fatalf("len(layers) = %d; want 2", len(received))
	}
	if received[0].Digest != "sha256:abc123" {
		t.Errorf("layer[0].Digest = %q; want sha256:abc123", received[0].Digest)
	}
	if received[1].Size != 2048 {
		t.Errorf("layer[1].Size = %d; want 2048", received[1].Size)
	}
}

func TestWriteLayer(t *testing.T) {
	blobsDir := t.TempDir()
	client, cleanup := startContainerServer(t, &mockContainerdClient{}, WithBlobsDir(blobsDir))
	defer cleanup()

	stream, err := client.WriteLayer(context.Background())
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	data := []byte("layer-data-part1")
	data2 := []byte("layer-data-part2")
	allData := append(data, data2...)
	h := sha256.Sum256(allData)
	hexStr := hex.EncodeToString(h[:])
	digest := "sha256:" + hexStr

	// Send first chunk with digest.
	if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: data}); err != nil {
		t.Fatalf("send chunk1: %v", err)
	}

	// Send second chunk.
	if err := stream.Send(&agentpb.WriteLayerRequest{Digest: digest, Data: data2}); err != nil {
		t.Fatalf("send chunk2: %v", err)
	}

	// Close send and receive response.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv response: %v", err)
	}

	blobPath := filepath.Join(blobsDir, "sha256", hexStr)
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("ReadFile %q: %v", blobPath, err)
	}
	if !bytes.Equal(got, allData) {
		t.Errorf("blob content = %q; want %q", got, allData)
	}
}

// ---- WriteLayer streaming-to-disk tests (Iteration 2) ----

// fakeWriteLayerServerStream implements grpc.BidiStreamingServer for testing
// receiveAndWriteLayer. Pre-populate msgs with the message sequence the stream
// should deliver; responses sent by the server accumulate in sent. endErr, if
// non-nil, is returned after all msgs are consumed instead of io.EOF.
type fakeWriteLayerServerStream struct {
	ctx    context.Context
	msgs   []*agentpb.WriteLayerRequest
	pos    int
	sent   []*agentpb.WriteLayerResponse
	endErr error
}

func (f *fakeWriteLayerServerStream) Recv() (*agentpb.WriteLayerRequest, error) {
	if f.pos >= len(f.msgs) {
		if f.endErr != nil {
			return nil, f.endErr
		}
		return nil, io.EOF
	}
	msg := f.msgs[f.pos]
	f.pos++
	return msg, nil
}

func (f *fakeWriteLayerServerStream) Send(r *agentpb.WriteLayerResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeWriteLayerServerStream) Context() context.Context     { return f.ctx }
func (f *fakeWriteLayerServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeWriteLayerServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeWriteLayerServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeWriteLayerServerStream) SendMsg(any) error            { return nil }
func (f *fakeWriteLayerServerStream) RecvMsg(any) error            { return nil }

// midTransferPauseWriteLayerStream wraps fakeWriteLayerServerStream and blocks
// on the second Recv call (after the first chunk has been processed) so the
// test goroutine can inspect the filesystem before the transfer completes.
type midTransferPauseWriteLayerStream struct {
	fakeWriteLayerServerStream
	paused chan struct{}
	resume chan struct{}
}

func (m *midTransferPauseWriteLayerStream) Recv() (*agentpb.WriteLayerRequest, error) {
	if m.pos == 1 {
		close(m.paused)
		<-m.resume
	}
	return m.fakeWriteLayerServerStream.Recv()
}

// writeLayerMsg builds a WriteLayerRequest with the given digest and data.
func writeLayerMsg(digest string, data []byte) *agentpb.WriteLayerRequest {
	return &agentpb.WriteLayerRequest{Digest: digest, Data: data}
}

// TestWriteLayer_TempFileExistsDuringTransfer verifies that a .partial.* temp
// file is created on disk as soon as the first chunk arrives and is still
// present while subsequent chunks are in-flight, before the transfer completes.
func TestWriteLayer_TempFileExistsDuringTransfer(t *testing.T) {
	blobsDir := t.TempDir()

	firstHalf := bytes.Repeat([]byte("z"), 32*1024)
	secondHalf := bytes.Repeat([]byte("z"), 32*1024)
	allContent := append(firstHalf, secondHalf...)
	h := sha256.Sum256(allContent)
	hexStr := hex.EncodeToString(h[:])
	digest := "sha256:" + hexStr

	paused := make(chan struct{})
	resume := make(chan struct{})
	stream := &midTransferPauseWriteLayerStream{
		fakeWriteLayerServerStream: fakeWriteLayerServerStream{
			ctx: context.Background(),
			msgs: []*agentpb.WriteLayerRequest{
				writeLayerMsg(digest, firstHalf),
				writeLayerMsg(digest, secondHalf),
			},
		},
		paused: paused,
		resume: resume,
	}

	svc := NewContainerService(zap.NewNop(), nil)
	errCh := make(chan error, 1)
	go func() { errCh <- svc.receiveAndWriteLayer(stream, blobsDir) }()

	<-paused

	matches, err := filepath.Glob(filepath.Join(blobsDir, hexStr+".partial.*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Error("no .partial.* temp file found while transfer is in-flight")
	}

	close(resume)

	if err := <-errCh; err != nil {
		t.Fatalf("receiveAndWriteLayer: %v", err)
	}
}

// TestWriteLayer_FinalBlobMatchesSourceContent verifies that after a
// successful transfer the file at <blobsDir>/sha256/<hex> is byte-for-byte
// identical to the data streamed across all chunks.
func TestWriteLayer_FinalBlobMatchesSourceContent(t *testing.T) {
	blobsDir := t.TempDir()

	content := bytes.Repeat([]byte("x"), 200*1024)
	h := sha256.Sum256(content)
	hexStr := hex.EncodeToString(h[:])
	digest := "sha256:" + hexStr

	const chunkSize = 64 * 1024
	var msgs []*agentpb.WriteLayerRequest
	for off := 0; off < len(content); off += chunkSize {
		end := off + chunkSize
		if end > len(content) {
			end = len(content)
		}
		msgs = append(msgs, writeLayerMsg(digest, content[off:end]))
	}

	stream := &fakeWriteLayerServerStream{ctx: context.Background(), msgs: msgs}
	svc := NewContainerService(zap.NewNop(), nil)

	if err := svc.receiveAndWriteLayer(stream, blobsDir); err != nil {
		t.Fatalf("receiveAndWriteLayer: %v", err)
	}

	blobPath := filepath.Join(blobsDir, "sha256", hexStr)
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("ReadFile %q: %v", blobPath, err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("blob content: got %d bytes, want %d", len(got), len(content))
	}
}

// TestWriteLayer_DigestMismatchReturnsErrorAndCleansUp verifies that when the
// SHA256 of the received bytes does not match the provided digest, the RPC
// returns a DataLoss error and no .partial.* or final blob file is left behind.
func TestWriteLayer_DigestMismatchReturnsErrorAndCleansUp(t *testing.T) {
	blobsDir := t.TempDir()

	wrongHex := strings.Repeat("0", 64)
	digest := "sha256:" + wrongHex

	stream := &fakeWriteLayerServerStream{
		ctx:  context.Background(),
		msgs: []*agentpb.WriteLayerRequest{writeLayerMsg(digest, []byte("some data"))},
	}
	svc := NewContainerService(zap.NewNop(), nil)

	err := svc.receiveAndWriteLayer(stream, blobsDir)
	if err == nil {
		t.Fatal("expected error on digest mismatch, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.DataLoss {
		t.Errorf("expected DataLoss, got: %v", err)
	}

	// No .partial.* files must remain.
	matches, _ := filepath.Glob(filepath.Join(blobsDir, "*.partial.*"))
	if len(matches) > 0 {
		t.Errorf(".partial.* files left behind: %v", matches)
	}

	// No blob file at the final path either.
	if _, err := os.Stat(filepath.Join(blobsDir, "sha256", wrongHex)); !errors.Is(err, os.ErrNotExist) {
		t.Error("blob file should not exist after digest mismatch")
	}
}

// TestWriteLayer_InterruptedStreamLeavesNoPartialFile verifies that if the
// stream terminates with an error before EOF, the RPC returns an error and
// no .partial.* file is left behind.
func TestWriteLayer_InterruptedStreamLeavesNoPartialFile(t *testing.T) {
	blobsDir := t.TempDir()

	fakeDigest := "sha256:" + strings.Repeat("0", 64)
	stream := &fakeWriteLayerServerStream{
		ctx:    context.Background(),
		msgs:   []*agentpb.WriteLayerRequest{writeLayerMsg(fakeDigest, []byte("chunk-1"))},
		endErr: status.Error(codes.Canceled, "client disconnected"),
	}
	svc := NewContainerService(zap.NewNop(), nil)

	if err := svc.receiveAndWriteLayer(stream, blobsDir); err == nil {
		t.Fatal("expected error on interrupted stream, got nil")
	}

	// No .partial.* files must remain.
	matches, _ := filepath.Glob(filepath.Join(blobsDir, "*.partial.*"))
	if len(matches) > 0 {
		t.Errorf(".partial.* files left behind after interrupted stream: %v", matches)
	}
}

// TestWriteLayer_BlobAlreadyExistsIsNoOp verifies that if the target blob
// already exists at <blobsDir>/sha256/<hex>, the transfer completes
// successfully and the existing file is left untouched.
func TestWriteLayer_BlobAlreadyExistsIsNoOp(t *testing.T) {
	blobsDir := t.TempDir()

	existingHex := strings.Repeat("a", 64)
	digest := "sha256:" + existingHex

	sha256Dir := filepath.Join(blobsDir, "sha256")
	if err := os.MkdirAll(sha256Dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	originalContent := []byte("original blob content")
	blobPath := filepath.Join(sha256Dir, existingHex)
	if err := os.WriteFile(blobPath, originalContent, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	stream := &fakeWriteLayerServerStream{
		ctx:  context.Background(),
		msgs: []*agentpb.WriteLayerRequest{writeLayerMsg(digest, []byte("new data that should be ignored"))},
	}
	svc := NewContainerService(zap.NewNop(), nil)

	if err := svc.receiveAndWriteLayer(stream, blobsDir); err != nil {
		t.Fatalf("receiveAndWriteLayer: %v", err)
	}

	// Existing blob must be unchanged.
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, originalContent) {
		t.Errorf("existing blob content changed; got %q, want %q", got, originalContent)
	}

	// No partial files.
	matches, _ := filepath.Glob(filepath.Join(blobsDir, "*.partial.*"))
	if len(matches) > 0 {
		t.Errorf(".partial.* files should not exist: %v", matches)
	}
}

func TestCreateContainerWithProgress(t *testing.T) {
	phases := []agentpb.CreateContainerProgress_Phase{
		agentpb.CreateContainerProgress_UNPACKING,
		agentpb.CreateContainerProgress_CREATING_CONTAINER,
		agentpb.CreateContainerProgress_COMPLETE,
	}
	mock := &mockContainerdClient{progressPhases: phases}
	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	stream, err := client.CreateContainerWithProgress(context.Background(), &agentpb.CreateContainerRequest{
		ImageName: "test-image:latest",
		AppName:   "test-app",
	})
	if err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	var receivedPhases []agentpb.CreateContainerProgress_Phase
	gotCompleted := false

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}

		switch r := resp.GetResponseType().(type) {
		case *agentpb.CreateContainerProgressResponse_Progress:
			receivedPhases = append(receivedPhases, r.Progress.GetPhase())
		case *agentpb.CreateContainerProgressResponse_Completed:
			gotCompleted = true
		}
	}

	if len(receivedPhases) != len(phases) {
		t.Fatalf("received %d progress phases; want %d", len(receivedPhases), len(phases))
	}
	for i, p := range receivedPhases {
		if p != phases[i] {
			t.Errorf("phase[%d] = %v; want %v", i, p, phases[i])
		}
	}
	if !gotCompleted {
		t.Error("did not receive Completed response")
	}
}

func TestAttachContainer(t *testing.T) {
	outputCh := make(chan ContainerOutput, 1)
	capturedAppCh := make(chan string, 1)
	stdinDataCh := make(chan string, 1)

	mock := &attachTestMock{
		onStartWithStdin: func(appName string, stdin io.Reader) (<-chan ContainerOutput, error) {
			capturedAppCh <- appName
			go func() {
				// Read all stdin bytes, then produce output so the server
				// doesn't close until we've verified what was forwarded.
				data, _ := io.ReadAll(stdin)
				stdinDataCh <- string(data)
				outputCh <- ContainerOutput{Stdout: []byte("pong")}
				close(outputCh)
			}()
			return outputCh, nil
		},
	}

	client, cleanup := startContainerServer(t, mock)
	defer cleanup()

	stream, err := client.AttachContainer(context.Background())
	if err != nil {
		t.Fatalf("AttachContainer: %v", err)
	}

	// First message must be app_name.
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_AppName{AppName: "echo-app"},
	}); err != nil {
		t.Fatalf("send app_name: %v", err)
	}

	// Verify StartContainerWithStdin was called with the correct app name.
	if got := <-capturedAppCh; got != "echo-app" {
		t.Errorf("appName = %q; want echo-app", got)
	}

	// Forward stdin data.
	if err := stream.Send(&agentpb.AttachContainerRequest{
		RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: []byte("ping")},
	}); err != nil {
		t.Fatalf("send stdin_data: %v", err)
	}

	// Close client send so the server's stdin reader reaches EOF, which unblocks
	// the mock goroutine's io.ReadAll and lets it produce output.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	// Expect Started response.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv started: %v", err)
	}
	if resp.GetStarted() == nil {
		t.Error("expected Started response")
	}

	// Expect stdout containing "pong".
	resp, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv stdout: %v", err)
	}
	if string(resp.GetStdoutOutput().GetData()) != "pong" {
		t.Errorf("stdout = %q; want pong", resp.GetStdoutOutput().GetData())
	}

	// Stream should end with EOF.
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("expected EOF; got %v", err)
	}

	// Confirm stdin bytes reached the container's stdin reader.
	if got := <-stdinDataCh; got != "ping" {
		t.Errorf("stdin data = %q; want ping", got)
	}
}
