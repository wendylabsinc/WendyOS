// Package chunkupload sends the chunks of one image layer that a device
// reports missing. The CLI (`wendy run`) and the build host (BuildService
// delivery) both use it, so the transport tuning and its failure handling live
// in one place (WDY-3211, WDY-2838).
package chunkupload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	grpcgzip "google.golang.org/grpc/encoding/gzip"

	"github.com/wendylabsinc/wendy/go/internal/shared/chunk"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// DefaultBatchChunks bounds one client-streaming WriteChunks RPC (at most
	// 16 MiB at chunk.MaxSize). CloseAndRecv acknowledges each batch; with
	// several streams in flight that acknowledgement no longer idles the link
	// the way 64-chunk stop-and-wait batches on a single stream did (WDY-3211:
	// ~35 MB/s on a 347 MB/s USB-C link).
	DefaultBatchChunks = 256

	// DefaultStreams is how many WriteChunks streams one layer uses at once.
	// The agent handles each stream on its own goroutine, so this is also how
	// many device cores inflate and stage one layer's chunks.
	DefaultStreams = 4

	// maxConcurrentStreams bounds WriteChunks streams across every layer this
	// process uploads at once. The budget is per PROCESS, not per target: in
	// the CLI, one process pushes to one device, so this also bounds streams
	// against that device's cores (the CLI pushes up to four layers
	// concurrently, so a multi-layer cold deploy does not open sixteen
	// streams against a four-core device). In the agent's build host, one
	// process can deliver to several devices at once, and those deliveries
	// share this same budget rather than each getting their own. A
	// per-connection budget is deferred to a follow-up.
	maxConcurrentStreams = 8
)

// Gzip is the gRPC compressor name for gzip-compressed WriteChunks messages.
const Gzip = grpcgzip.Name

// streamSlots is the process-wide stream budget; see maxConcurrentStreams.
var streamSlots = make(chan struct{}, maxConcurrentStreams)

// Options tunes one layer's upload.
type Options struct {
	// Layer names the layer, normally its diff ID, in error messages.
	Layer string
	// BatchChunks is the number of chunks per WriteChunks stream; <= 0 means
	// DefaultBatchChunks.
	BatchChunks int
	// Streams is the number of concurrent WriteChunks streams for this layer;
	// <= 0 means DefaultStreams.
	Streams int
	// Compressor is the gRPC compressor for WriteChunks messages, such as
	// Gzip. Empty sends chunk bytes uncompressed.
	Compressor string
	// OnSent, when set, is called with each chunk's length once its Send
	// returns. It is called from several goroutines at once.
	OnSent func(n int)
	// Activity, when set, records stream opens and progress for a stall
	// watchdog (see Watch). One Activity is shared by every layer of a push.
	Activity *Activity
}

// Plan returns the chunks in refs whose hashes are in missing, each hash once,
// in first-occurrence order, and their total length. A layer can contain the
// same content more than once; one staged copy satisfies every occurrence.
func Plan(refs []chunk.Ref, missing map[[32]byte]bool) ([]chunk.Ref, int64) {
	var (
		plan  []chunk.Ref
		total int64
	)
	planned := make(map[[32]byte]bool, len(missing))
	for _, ref := range refs {
		if !missing[ref.Hash] || planned[ref.Hash] {
			continue
		}
		planned[ref.Hash] = true
		plan = append(plan, ref)
		total += int64(ref.Len)
	}
	return plan, total
}

// Upload sends every chunk in plan, reading its bytes from src at the chunk's
// offset. Batches of opts.BatchChunks chunks are handed out in plan order to up
// to opts.Streams concurrent WriteChunks streams, so the device receives a
// layer roughly front to back. The first failure cancels the other streams and
// is returned.
func Upload(ctx context.Context, cs agentpb.WendyContainerServiceClient, src io.ReaderAt, plan []chunk.Ref, opts Options) error {
	if len(plan) == 0 {
		return nil
	}
	batch := opts.BatchChunks
	if batch <= 0 {
		batch = DefaultBatchChunks
	}
	streams := opts.Streams
	if streams <= 0 {
		streams = DefaultStreams
	}
	batches := (len(plan) + batch - 1) / batch
	streams = min(streams, batches)

	var next atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	for range streams {
		g.Go(func() error {
			for {
				b := int(next.Add(1) - 1)
				if b >= batches {
					return nil
				}
				start := b * batch
				if err := sendBatch(gctx, cs, src, plan, start, min(start+batch, len(plan)), opts); err != nil {
					return err
				}
			}
		})
	}
	return g.Wait()
}

// sendBatch sends plan[start:end] over one WriteChunks stream and waits for
// the device to acknowledge it.
func sendBatch(ctx context.Context, cs agentpb.WendyContainerServiceClient, src io.ReaderAt, plan []chunk.Ref, start, end int, opts Options) error {
	select {
	case streamSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-streamSlots }()

	// A failed batch returns without CloseAndRecv; cancelling tears its stream
	// down now rather than when the whole upload ends.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var callOpts []grpc.CallOption
	if opts.Compressor != "" {
		callOpts = append(callOpts, grpc.UseCompressor(opts.Compressor))
	}
	wc, err := cs.WriteChunks(ctx, callOpts...)
	if err != nil {
		return fmt.Errorf("opening chunk upload for layer %s: %w", opts.Layer, err)
	}
	opts.Activity.opened()
	defer opts.Activity.closed()
	for i := start; i < end; i++ {
		ref := plan[i]
		// A fresh buffer per message: gRPC may still hold the previous one
		// after Send returns (stats handlers read messages lazily).
		buf := make([]byte, ref.Len)
		// io.ReaderAt's contract permits n == len(buf) together with io.EOF
		// for a read that reaches the end of the input (e.g. the last chunk
		// of a layer); only a short read is an actual failure.
		if n, err := src.ReadAt(buf, int64(ref.Offset)); err != nil && !(n == len(buf) && errors.Is(err, io.EOF)) {
			return fmt.Errorf("reading chunk %d/%d for layer %s: %w", i+1, len(plan), opts.Layer, err)
		}
		hash := ref.Hash
		if err := wc.Send(&agentpb.WriteChunksRequest{Hash: hash[:], Data: buf}); err != nil {
			// grpc-go reports io.EOF from Send once the server has closed the
			// stream. CloseAndRecv carries the real status (ResourceExhausted,
			// InvalidArgument, Unavailable), which callers and the resume logic
			// act on; a bare EOF would read as a transport drop.
			if errors.Is(err, io.EOF) {
				if _, terminal := wc.CloseAndRecv(); terminal != nil {
					err = terminal
				}
			}
			return fmt.Errorf("sending chunk %d/%d for layer %s: %w", i+1, len(plan), opts.Layer, err)
		}
		opts.Activity.progressed()
		if opts.OnSent != nil {
			opts.OnSent(len(buf))
		}
	}
	if _, err := wc.CloseAndRecv(); err != nil {
		return fmt.Errorf("confirming chunks %d-%d of %d for layer %s: %w", start+1, end, len(plan), opts.Layer, err)
	}
	opts.Activity.progressed()
	return nil
}
