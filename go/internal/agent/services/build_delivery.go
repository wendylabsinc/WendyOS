package services

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// This file is the build host's DELIVERY leg: getting an image it has just
// built onto another device. It is the same mechanism `wendy run` uses from a
// laptop (see the CLI's pushLayersByChunks), running on the build host instead:
// ask the device which layers and chunks it already holds, send only the bytes
// it lacks, and have it assemble the image itself.
//
// The registry push this replaces sent every layer whole, and a single dropped
// connection lost the entire transfer — on a long-haul link (a US build host
// delivering to a device in Canada) that was the dominant failure, with every
// retry starting from zero. Delivering into the device's content-addressed
// chunk store gives two properties a push cannot:
//
//   - Resume: a transfer that dies at 60% continues from the chunks the device
//     already staged. Nothing is sent twice.
//   - Dedup: layers unchanged since the last deploy — base image, CUDA, model
//     weights — never cross the link at all.
//
// See WDY-2605.

// DefaultTargetAgentPort is the mTLS gRPC port a WendyOS agent listens on
// (WENDY_AGENT_PORT + 1), and so where a target's chunk store is reached.
const DefaultTargetAgentPort uint16 = 50052

// errChunkDeliveryUnsupported reports that the target's agent predates the RPCs
// chunked delivery needs (QueryChunks, or PrepareImage to register the
// assembled image under a name). It is the ONE delivery failure that falls back
// to a registry push; every other failure is reported, because a fallback that
// also caught genuine errors would retry an unrecoverable delivery over a
// slower path and then report the wrong reason.
var errChunkDeliveryUnsupported = errors.New("the device's agent predates chunked delivery")

const (
	// deliveryAttempts bounds how many times one delivery is resumed after a
	// transport drop. Each resume re-queries what the device holds, so a
	// genuinely dead link costs a few round trips, not a re-sent image.
	deliveryAttempts = 4
	// maxConcurrentDeliveryLayers bounds layers decompressed and streamed at
	// once. Mirrors the CLI's maxConcurrentLayerPush: it overlaps one layer's
	// CPU work with another's network round trips, and each in-flight layer
	// spills its uncompressed tar to disk rather than RAM.
	maxConcurrentDeliveryLayers = 4
	// maxChunksPerDeliveryStream bounds one client-streaming WriteChunks RPC.
	// Closing a batch gets an application-level acknowledgement, so at most
	// this many chunks (4 MiB) can be in flight unconfirmed when a link drops.
	maxChunksPerDeliveryStream = 64
	// deliveryProgressInterval paces the byte-count lines sent to the CLI.
	deliveryProgressInterval = time.Second
	// deliveryVertexBase numbers the synthetic BuildKit progress vertex one
	// delivery reports through. BuildKit numbers its own vertexes from 1, and a
	// build with hundreds of them is not a real build, so these never collide.
	deliveryVertexBase = 900
	// exportedImageSuffix names the OCI tar buildctl exports, as a sibling of
	// the app's context directory.
	exportedImageSuffix = ".image.tar"
)

// targetDialer opens a gRPC connection to a target device's agent. A field on
// the service rather than a method so tests can point delivery at an
// in-process fake; production dials the mesh peer, pinned to its asset id.
type targetDialer func(ctx context.Context, target *agentpbv2.PushTarget) (*grpc.ClientConn, error)

// meshTargetDialer reaches a target's agent the way the registry push reached
// its registry: a byte stream from the peer dialer, LAN-direct or relayed,
// with mTLS on top pinned to the asset the image was built for. The pin is
// not optional — see BuildServiceOptions.PushTLS — and it holds for this hop
// exactly as for the registry one, since the image content is the same.
func meshTargetDialer(peers PeerDialer, pushTLS func(int32) (*tls.Config, error), defaultPort uint16) targetDialer {
	return func(ctx context.Context, target *agentpbv2.PushTarget) (*grpc.ClientConn, error) {
		if peers == nil || pushTLS == nil {
			return nil, errors.New("this build host has no mesh dialer or client certificate")
		}
		port, err := deliveryTargetAgentPort(target, defaultPort)
		if err != nil {
			return nil, err
		}
		tlsCfg, err := pushTLS(target.GetAssetId())
		if err != nil {
			return nil, fmt.Errorf("loading delivery credentials: %w", err)
		}
		assetID := target.GetAssetId()
		return grpc.NewClient("passthrough:///build-target",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				conn, _, err := peers.DialDevice(ctx, assetID, port)
				return conn, err
			}),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
			// The device unpacks the image after the last chunk lands, which can
			// take minutes with nothing on the wire. Pings keep a relayed path
			// from being torn down as idle meanwhile; the agent's server policy
			// permits one every 10s, so 30s is well within it.
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:    30 * time.Second,
				Timeout: 20 * time.Second,
			}),
		)
	}
}

func deliveryTargetAgentPort(target *agentpbv2.PushTarget, fallback uint16) (uint16, error) {
	port := target.GetAgentPort()
	if port == 0 {
		return fallback, nil
	}
	if port > 65535 {
		return 0, fmt.Errorf("target device %d has invalid agent port %d", target.GetAssetId(), port)
	}
	return uint16(port), nil
}

// targetImageName is the reference the target's own registry would have stored
// a pushed image under — localhost:<registry port>/<repository> — and therefore
// the one the CLI's CreateContainer asks for afterwards. Registering the
// assembled image under the same name is what lets the CLI stay unchanged:
// it cannot tell which way the image arrived, and does not need to.
func targetImageName(t *agentpbv2.PushTarget) string {
	return fmt.Sprintf("localhost:%d/%s", t.GetRegistryPort(), t.GetRepository())
}

// buildProgress serialises writes to the BuildImage stream. buildctl's output is
// forwarded from one goroutine, but a delivery reports from several — one per
// in-flight layer plus the preparation wait — and a gRPC server stream permits
// one Send at a time.
type buildProgress struct {
	mu     sync.Mutex
	stream agentpbv2.WendyBuildService_BuildImageServer
}

// logf sends one log line. A failed send is dropped rather than returned: it
// means the CLI has gone, and the stream context that cancels the delivery
// itself is the signal for that.
func (p *buildProgress) logf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.stream.Send(&agentpbv2.BuildImageProgress{
		Event: &agentpbv2.BuildImageProgress_LogLine{LogLine: fmt.Sprintf(format, args...)},
	})
}

// deliveryReporter renders one delivery as a BuildKit plain-progress vertex, so
// the CLI's existing build renderer shows it live without learning a new
// format. The vertex is named "pushing ...", which the renderer folds into its
// "exporting + pushing layers" row — the same place the registry push
// reported, and where PR #1771's users learned to look. Byte lines take the
// "<sent> / <total>" shape the renderer already sniffs for transfer counters;
// detail lines take BuildKit's "<elapsed> <text>" log shape.
type deliveryReporter struct {
	prog    *buildProgress
	vertex  string
	started time.Time

	mu    sync.Mutex
	total int64
	sent  int64
	last  time.Time
}

func newDeliveryReporter(prog *buildProgress, index int, assetID int32) *deliveryReporter {
	r := &deliveryReporter{
		prog:    prog,
		vertex:  fmt.Sprintf("#%d", deliveryVertexBase+index),
		started: time.Now(),
	}
	r.prog.logf("%s pushing layers to device %d by chunks", r.vertex, assetID)
	return r
}

func (r *deliveryReporter) detail(format string, args ...any) {
	r.prog.logf("%s %.3f %s", r.vertex, time.Since(r.started).Seconds(), fmt.Sprintf(format, args...))
}

// resetTransfer starts the byte counters over for a resumed attempt, so the
// counters describe what this attempt still has to send rather than
// double-counting the bytes that landed before the drop.
func (r *deliveryReporter) resetTransfer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total, r.sent, r.last = 0, 0, time.Time{}
}

func (r *deliveryReporter) planned(n int64) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += n
	r.reportLocked(true)
}

func (r *deliveryReporter) advanced(n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent += int64(n)
	r.reportLocked(r.sent >= r.total)
}

func (r *deliveryReporter) sentBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

func (r *deliveryReporter) reportLocked(force bool) {
	now := time.Now()
	if !force && !r.last.IsZero() && now.Sub(r.last) < deliveryProgressInterval {
		return
	}
	r.last = now
	r.prog.logf("%s sending %s / %s", r.vertex, formatBuildBytes(r.sent), formatBuildBytes(r.total))
}

func (r *deliveryReporter) done() {
	r.prog.logf("%s DONE %.1fs", r.vertex, time.Since(r.started).Seconds())
}

// formatBuildBytes renders a byte count in the units BuildKit's plain progress
// uses, which is what the CLI's renderer parses.
func formatBuildBytes(n int64) string {
	const unit = 1024.0
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f%s", v, suffix)
		}
	}
	return fmt.Sprintf("%.1fTiB", v/unit)
}

// deliveryLayer is one layer resolved for delivery: its reassembly header, and
// — when the device may need bytes from it — the uncompressed tar on disk that
// chunk bytes are read back from.
type deliveryLayer struct {
	header *agentpb.RunContainerLayerHeader
	refs   []chunk.Ref
	f      *os.File
}

func (l *deliveryLayer) close() {
	if l.f == nil {
		return
	}
	name := l.f.Name()
	_ = l.f.Close()
	_ = os.Remove(name)
	l.f = nil
}

// closeDeliveryLayers removes every scratch file a build resolved. Called once
// per build, after the last device, so a fleet shares one decompression.
func closeDeliveryLayers(resolved map[string]*deliveryLayer) {
	for _, l := range resolved {
		l.close()
	}
}

// decompressAndChunkLayer streams one layer out of the exported tar, through
// its decompressor, into a file under dir, content-chunking and hashing the
// uncompressed bytes in the same pass. Peak memory is the decompressor window
// plus chunking buffers, never the layer.
//
// dir is the build state directory rather than the system temp dir on purpose:
// /tmp is tmpfs on WendyOS, and a multi-GiB layer spilled there is a layer
// spilled into RAM.
func decompressAndChunkLayer(img *exportedImage, l ociLayer, dir string) (*deliveryLayer, error) {
	src, err := os.Open(img.tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening the exported image: %w", err)
	}
	defer src.Close()
	raw, release, err := layerDecompressor(io.NewSectionReader(src, l.offset, l.size), l.mediaType)
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", l.diffID, err)
	}
	defer release()

	f, err := os.CreateTemp(dir, "deliver-layer-*")
	if err != nil {
		return nil, fmt.Errorf("creating layer scratch file: %w", err)
	}
	discard := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	h := sha256.New()
	refs, n, err := chunk.ChunkStream(io.TeeReader(raw, io.MultiWriter(f, h)), nil)
	if err != nil {
		discard()
		return nil, fmt.Errorf("decompressing and chunking layer %s: %w", l.diffID, err)
	}
	// The config's diff ID is what the device will verify the reassembled layer
	// against. Catching a mismatch here, before a byte crosses the mesh, turns a
	// failed assembly on the device into a clear error on the host.
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != l.diffID {
		discard()
		return nil, fmt.Errorf("layer %s decompressed to %s; the exported image is inconsistent", l.diffID, got)
	}
	hashes := make([][]byte, len(refs))
	for i, ref := range refs {
		hh := ref.Hash
		hashes[i] = hh[:]
	}
	return &deliveryLayer{
		header: &agentpb.RunContainerLayerHeader{
			Digest:      l.diffID,
			DiffId:      l.diffID,
			Size:        n,
			Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE,
			ChunkHashes: hashes,
		},
		refs: refs,
		f:    f,
	}, nil
}

// deliverByChunks delivers img to one device by chunks, resuming across
// transport drops.
//
// Resume needs no bookkeeping of its own: WriteChunks stages each chunk on the
// device as it lands, and the next attempt's QueryChunks reports only what is
// still missing.
//
// resolved is the build's cache of decompressed, chunked layers, keyed by diff
// ID and owned by the caller: layers resolved for one attempt serve the next,
// and layers resolved for one device serve the rest of the fleet, so a resume
// costs round trips and a second device costs a diff — not another
// decompression. Deliveries share it sequentially; it is not safe for
// concurrent callers.
func (s *BuildService) deliverByChunks(ctx context.Context, prog *buildProgress, index int, img *exportedImage, target *agentpbv2.PushTarget, resolved map[string]*deliveryLayer) error {
	rep := newDeliveryReporter(prog, index, target.GetAssetId())

	for attempt := 1; ; attempt++ {
		err := s.deliverByChunksOnce(ctx, rep, img, target, resolved)
		if err == nil {
			rep.done()
			return nil
		}
		if attempt == deliveryAttempts || !retryableDeliveryError(err) {
			return err
		}
		rep.detail("transfer interrupted after %s (%v); resuming — chunks the device already holds are not re-sent (attempt %d/%d)",
			formatBuildBytes(rep.sentBytes()), err, attempt+1, deliveryAttempts)
		if err := sleepContext(ctx, deliveryBackoff(attempt)); err != nil {
			return err
		}
		rep.resetTransfer()
	}
}

// deliveryBackoff is the pause before resuming after the given failed attempt:
// 1s, 2s, 4s. A variable so tests can run the resume loop without waiting.
var deliveryBackoff = func(attempt int) time.Duration {
	return time.Duration(1<<(attempt-1)) * time.Second
}

// deliverByChunksOnce is one attempt: connect, learn what the device holds,
// send the rest, and have it assemble the image. resolved caches decompressed
// layers by diff ID across attempts and is owned by the caller.
func (s *BuildService) deliverByChunksOnce(ctx context.Context, rep *deliveryReporter, img *exportedImage, target *agentpbv2.PushTarget, resolved map[string]*deliveryLayer) error {
	assetID := target.GetAssetId()
	cc, err := s.dialTarget(ctx, target)
	if err != nil {
		return &transientDeliveryError{fmt.Errorf("reaching device %d over the mesh: %w", assetID, err)}
	}
	defer cc.Close()
	cs := agentpb.NewWendyContainerServiceClient(cc)

	// Capability probe, before any layer is decompressed: an agent without a
	// chunk store answers Unimplemented, and the caller falls back to a registry
	// push instead of wasting a layer's worth of work first.
	if _, err := cs.QueryChunks(ctx, &agentpb.QueryChunksRequest{}); err != nil {
		if code, _ := grpcStatusCode(err); code == codes.Unimplemented {
			return errChunkDeliveryUnsupported
		}
		return fmt.Errorf("querying device %d's chunk store: %w", assetID, err)
	}
	present, err := presentLayersOn(ctx, cs, img)
	if err != nil {
		rep.detail("could not ask device %d which layers it already holds (%v); chunking every layer", assetID, err)
	}

	// Classify every layer before resolving any, so the cache of decompressed
	// layers is read and written from one goroutine.
	headers := make([]*agentpb.RunContainerLayerHeader, len(img.layers))
	var toPush []*deliveryLayer
	var toResolve []int
	reused := 0
	for i, l := range img.layers {
		if size, ok := present[l.diffID]; ok {
			// No chunk hashes: the device reuses the blob it already has, and is
			// never asked for bytes it would then not need.
			headers[i] = &agentpb.RunContainerLayerHeader{
				Digest:      l.diffID,
				DiffId:      l.diffID,
				Size:        size,
				Compression: agentpb.RunContainerLayerHeader_COMPRESSION_NONE,
			}
			reused++
			continue
		}
		if dl, ok := resolved[l.diffID]; ok {
			headers[i] = dl.header
			toPush = append(toPush, dl)
			continue
		}
		toResolve = append(toResolve, i)
	}
	if len(toResolve) > 0 {
		fresh := make([]*deliveryLayer, len(img.layers))
		var g errgroup.Group
		g.SetLimit(maxConcurrentDeliveryLayers)
		for _, i := range toResolve {
			g.Go(func() error {
				dl, err := decompressAndChunkLayer(img, img.layers[i], s.stateDir)
				if err != nil {
					return err
				}
				fresh[i] = dl // distinct index per goroutine
				return nil
			})
		}
		waitErr := g.Wait()
		for _, i := range toResolve {
			if dl := fresh[i]; dl != nil {
				resolved[img.layers[i].diffID] = dl
				headers[i] = dl.header
				toPush = append(toPush, dl)
			}
		}
		if waitErr != nil {
			return fmt.Errorf("preparing layers for device %d: %w", assetID, waitErr)
		}
	}
	rep.detail("%d layer(s): device %d already has %d, diffing %d", len(img.layers), assetID, reused, len(toPush))

	// PrepareImage runs concurrently with the upload. On the device it waits for
	// each layer's chunks, then assembles and unpacks from the base up while
	// later layers are still arriving; it returns once the image is registered
	// under its name. It is strict here, as for Compose: the CLI will create the
	// container by NAME, so an image that was never registered is a failed
	// delivery, not something RunContainer can finish later.
	imageName := targetImageName(target)
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	uploadCtx, cancelUpload := context.WithCancel(ctx)
	defer cancelUpload()
	prepareDone := make(chan error, 1)
	go func() {
		_, err := cs.PrepareImage(prepareCtx, &agentpb.RunContainerLayersRequest{
			ImageName:   imageName,
			Layers:      headers,
			ImageConfig: img.config,
		})
		prepareDone <- err
		if err != nil {
			// Stop sending as soon as the device says it cannot assemble; without
			// this the whole delta is uploaded before the error is discovered.
			cancelUpload()
		}
	}()

	if len(toPush) > 0 {
		g, gctx := errgroup.WithContext(uploadCtx)
		g.SetLimit(maxConcurrentDeliveryLayers)
		for _, dl := range toPush {
			g.Go(func() error { return uploadLayerChunks(gctx, cs, dl, rep) })
		}
		if uploadErr := g.Wait(); uploadErr != nil {
			return settleUploadFailure(assetID, uploadErr, prepareDone, cancelPrepare)
		}
	}

	select {
	case prepareErr := <-prepareDone:
		return classifyPrepareError(assetID, prepareErr)
	default:
		rep.detail("waiting for device %d to assemble and unpack the image", assetID)
		return classifyPrepareError(assetID, <-prepareDone)
	}
}

// settleUploadFailure decides what an upload failure means once preparation
// is accounted for. Preparation may have failed first — it cancels the upload
// when it does, so its error is the real one and the upload's is just "context
// canceled". It may still be running, in which case it is cancelled and
// awaited so the goroutine does not outlive the attempt. Or it may already
// have finished: when every chunk was staged by an earlier attempt the device
// registers the image at once, and the upload's own QueryChunks or
// CloseAndRecv can still lose the link afterwards. The goroutine reports
// exactly once, so a second wait in that case would hang the RPC for good.
func settleUploadFailure(assetID int32, uploadErr error, prepareDone <-chan error, cancelPrepare context.CancelFunc) error {
	select {
	case prepareErr := <-prepareDone:
		if prepareErr != nil {
			return classifyPrepareError(assetID, prepareErr)
		}
		return uploadErr
	default:
	}
	cancelPrepare()
	<-prepareDone
	return uploadErr
}

// presentLayersOn asks the device which layers it already holds, by diff ID.
// Purely an optimisation: on any failure, including an agent too old for
// QueryLayers, the caller chunks every layer as before — but says so, since a
// pre-check that fails on the link the delivery is about to use is worth a
// line in the log.
func presentLayersOn(ctx context.Context, cs agentpb.WendyContainerServiceClient, img *exportedImage) (map[string]int64, error) {
	ids := make([]string, 0, len(img.layers))
	for _, l := range img.layers {
		ids = append(ids, l.diffID)
	}
	resp, err := cs.QueryLayers(ctx, &agentpb.QueryLayersRequest{DiffIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(resp.GetPresent()))
	for _, p := range resp.GetPresent() {
		out[p.GetDiffId()] = p.GetSize()
	}
	return out, nil
}

// uploadLayerChunks sends the chunks of one layer the device reports missing.
func uploadLayerChunks(ctx context.Context, cs agentpb.WendyContainerServiceClient, dl *deliveryLayer, rep *deliveryReporter) error {
	diffID := dl.header.GetDiffId()
	q, err := cs.QueryChunks(ctx, &agentpb.QueryChunksRequest{ChunkHashes: dl.header.GetChunkHashes()})
	if err != nil {
		return fmt.Errorf("querying missing chunks of layer %s: %w", diffID, err)
	}
	missing := make(map[[32]byte]bool, len(q.GetMissingHashes()))
	for _, hb := range q.GetMissingHashes() {
		var h [32]byte
		copy(h[:], hb)
		missing[h] = true
	}
	var missingBytes int64
	planned := make(map[[32]byte]bool, len(missing))
	for _, ref := range dl.refs {
		if !missing[ref.Hash] || planned[ref.Hash] {
			continue
		}
		planned[ref.Hash] = true
		missingBytes += int64(ref.Len)
	}
	rep.planned(missingBytes)
	if len(missing) == 0 {
		return nil
	}

	var wc grpc.ClientStreamingClient[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]
	inStream := 0
	for i, ref := range dl.refs {
		if !missing[ref.Hash] {
			continue
		}
		if wc == nil {
			// Compressed on the wire, as the CLI sends them: gRPC inflates the
			// message before the agent hashes and stages the original bytes.
			wc, err = cs.WriteChunks(ctx, grpc.UseCompressor(grpcgzip.Name))
			if err != nil {
				return fmt.Errorf("opening chunk upload for layer %s: %w", diffID, err)
			}
		}
		buf := make([]byte, ref.Len) // ref.Len <= chunk.MaxSize (64 KiB)
		if _, err := dl.f.ReadAt(buf, int64(ref.Offset)); err != nil {
			return fmt.Errorf("reading chunk %d/%d of layer %s: %w", i+1, len(dl.refs), diffID, err)
		}
		hb := ref.Hash
		if err := wc.Send(&agentpb.WriteChunksRequest{Hash: hb[:], Data: buf}); err != nil {
			// Send reports io.EOF once the server has closed the stream; the
			// terminal status — ResourceExhausted, InvalidArgument, Unavailable —
			// comes from CloseAndRecv, and is the one worth reporting and the
			// one the resume decision is made on.
			if errors.Is(err, io.EOF) {
				if _, terminal := wc.CloseAndRecv(); terminal != nil {
					err = terminal
				}
			}
			return fmt.Errorf("sending chunk %d/%d of layer %s: %w", i+1, len(dl.refs), diffID, err)
		}
		// The ordered manifest may reference identical content more than once.
		// One staged copy satisfies every occurrence, so do not spend network or
		// progress budget sending it again during this attempt.
		delete(missing, ref.Hash)
		rep.advanced(len(buf))
		inStream++
		if inStream == maxChunksPerDeliveryStream {
			if _, err := wc.CloseAndRecv(); err != nil {
				return fmt.Errorf("confirming chunk upload batch for layer %s: %w", diffID, err)
			}
			wc, inStream = nil, 0
		}
	}
	if wc != nil {
		if _, err := wc.CloseAndRecv(); err != nil {
			return fmt.Errorf("confirming chunk upload for layer %s: %w", diffID, err)
		}
	}
	return nil
}

// classifyPrepareError maps the device's answer to PrepareImage. Unimplemented
// means the agent cannot register an image by name from chunks, which is the
// fallback case; anything else is that device's failure, and its gRPC code is
// preserved through the wrap so the resume decision can read it.
func classifyPrepareError(assetID int32, err error) error {
	if err == nil {
		return nil
	}
	if code, _ := grpcStatusCode(err); code == codes.Unimplemented {
		return errChunkDeliveryUnsupported
	}
	return fmt.Errorf("device %d could not assemble the image from chunks: %w", assetID, err)
}

// transientDeliveryError marks a failure that happened before any bytes moved
// — a dial that did not come up — so the resume loop retries it as it would a
// drop mid-transfer.
type transientDeliveryError struct{ err error }

func (e *transientDeliveryError) Error() string { return e.err.Error() }
func (e *transientDeliveryError) Unwrap() error { return e.err }

// retryableDeliveryError decides whether a failed attempt is resumed.
//
// Resumed: a dial that failed, a gRPC Unavailable (the transport reporting the
// link is down), and an EOF-shaped stream death, which is how a broken HTTP/2
// stream surfaces when the drop outruns the tunnel's own status framing.
//
// Not resumed: cancellation — the CLI hung up or a deadline passed, and
// retrying would fight that rather than honour it; an unsupported agent, which
// no retry changes; and every other status, which is the device saying
// something specific about this image (too large, malformed, refused) that
// re-sending cannot fix.
func retryableDeliveryError(err error) bool {
	if err == nil || errors.Is(err, errChunkDeliveryUnsupported) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var transient *transientDeliveryError
	if errors.As(err, &transient) {
		return true
	}
	if code, ok := grpcStatusCode(err); ok {
		return code == codes.Unavailable
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// grpcStatusCode finds the gRPC status in an error chain, if there is one.
func grpcStatusCode(err error) (codes.Code, bool) {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if st, ok := status.FromError(cur); ok {
			return st.Code(), true
		}
	}
	return codes.Unknown, false
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
