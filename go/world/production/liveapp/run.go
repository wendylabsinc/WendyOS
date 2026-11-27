package liveapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

func readFile(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfig
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrConfig
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > limit {
		return nil, ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(raw) > int(limit) {
		return nil, ErrConfig
	}
	return raw, nil
}
func LoadConfig(path string) (Config, error) {
	raw, err := readFile(path, 16<<10)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if jws.DecodeStrict(raw, &cfg) != nil {
		return cfg, ErrConfig
	}
	return cfg.normalized()
}

// Dial uses only the explicitly mounted Unix socket. The server binds the
// kernel process identity to its commissioned service. There is no admin API.
func Dial(path string) (*grpc.ClientConn, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 103 {
		return nil, ErrConfig
	}
	return grpc.NewClient("passthrough:///world-live-sensor", grpc.WithTransportCredentials(appservice.ClientCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}), grpc.WithDisableRetry(), grpc.WithDefaultCallOptions(grpc.MaxRetryRPCBufferSize(0), grpc.MaxCallRecvMsgSize(256<<10), grpc.MaxCallSendMsgSize(64<<10)))
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrUncertain):
		return "mutation_outcome_unknown"
	case errors.Is(err, livecapture.ErrBootChanged):
		return "scene_boot_changed"
	case errors.Is(err, livecapture.ErrStale):
		return "capture_stale"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrConfig):
		return "configuration_refused"
	default:
		return "capture_or_acknowledgment_refused"
	}
}

// Run uses one livecapture.Client for the whole run. Its boot, sequence and
// original capture-age checks remain in force even for skipped duplicates.
func Run(ctx context.Context, config Config, api appv1.WorldAppServiceClient) (result Result, err error) {
	result = Result{Version: 1, RunID: config.RunID, SensorID: config.SensorID, Status: "failed", Receipts: []Receipt{}}
	defer func() {
		if err != nil {
			result.ErrorCode = errorCode(err)
			if errors.Is(err, ErrUncertain) {
				result.Status = "unknown_outcome"
			}
		}
	}()
	config, err = config.normalized()
	if err != nil || ctx == nil {
		return result, ErrConfig
	}
	token, err := readFile(config.ReadTokenFile, 257)
	if err != nil {
		return result, err
	}
	// Permit one conventional trailing newline in a mounted token file.
	token = bytes.TrimSuffix(token, []byte("\n"))
	if strings.ContainsAny(string(token), "\r\n") {
		return result, ErrConfig
	}
	client, err := livecapture.New(livecapture.Config{URL: config.SceneURL, Token: string(token), RunID: config.RunID, Site: config.Site, Frame: config.Frame, MaxAge: 500 * time.Millisecond})
	clear(token)
	if err != nil {
		return result, ErrConfig
	}
	publisher, err := NewPublisher(api, PublisherConfig{RequestTimeout: time.Duration(config.RequestTimeoutMS) * time.Millisecond})
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.DurationMS)*time.Millisecond)
	defer cancel()
	var previous uint64
	for {
		snapshot, e := client.Capture(ctx)
		if e != nil {
			return result, e
		}
		if snapshot.Sequence == previous {
			result.Duplicates++
		} else {
			request, media, e := sensorrecord.FromSnapshot(snapshot, config.SensorID, time.Unix(0, config.SimulationAnchorUnixNS).UTC(), time.Duration(config.TTLMS)*time.Millisecond)
			if e != nil {
				return result, ErrCapture
			}
			receipt, e := publisher.Submit(ctx, request, media)
			if e != nil {
				return result, e
			}
			result.Receipts = append(result.Receipts, receipt)
			previous = snapshot.Sequence
			if len(result.Receipts) == config.MaxSamples {
				result.Status = "complete"
				return result, nil
			}
		}
		// Wait after the operation so a slow upload cannot accumulate timer
		// ticks and cause a burst of publications when it completes.
		timer := time.NewTimer(time.Duration(config.PollIntervalMS) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}
