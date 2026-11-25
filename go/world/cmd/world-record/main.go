// world-record captures a bounded history from a simulation replica and its
// explicit evidence endpoints. It is a polling recorder, not a lossless journal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/recording"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var errCaptureComplete = errors.New("recording duration completed")

type config struct {
	Reader          client.Config   `json:"reader"`
	EvidenceSources []client.Config `json:"evidence_sources"`
}

func readConfig(path string) (config, error) {
	var cfg config
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return cfg, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return cfg, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return cfg, errors.New("recorder config must be a private regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if e != nil {
		return cfg, e
	}
	e = authority.DecodeStrict(data, &cfg)
	return cfg, e
}
func record(ctx context.Context, args []string, output io.Writer) error {
	f := flag.NewFlagSet("world-record", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	cfgPath := f.String("config", "", "private recorder configuration")
	out := f.String("out", "", "new recording directory")
	runID := f.String("run-id", "", "original simulation run")
	frameID := f.String("frame", "office-floor", "original frame ID")
	revision := f.String("frame-revision", "office-frame-v1", "original frame revision")
	duration := f.Duration("duration", 5*time.Second, "bounded polling duration")
	interval := f.Duration("interval", 100*time.Millisecond, "snapshot interval")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *cfgPath == "" || *out == "" || *runID == "" || *frameID == "" || *revision == "" || f.NArg() != 0 || *duration < 100*time.Millisecond || *duration > time.Minute || *interval < 20*time.Millisecond || *interval > 5*time.Second || *interval > *duration {
		return errors.New("config, new out, original run/frame, duration [100ms,1m] and interval [20ms,5s] required")
	}
	if _, e := os.Lstat(*out); !errors.Is(e, os.ErrNotExist) {
		if e != nil {
			return e
		}
		return os.ErrExist
	}
	cfg, e := readConfig(*cfgPath)
	if e != nil {
		return e
	}
	grant := cfg.Reader.Grant.Grant
	if grant.Domain != "simulation" || len(cfg.EvidenceSources) < 1 || len(cfg.EvidenceSources) > 8 {
		return errors.New("recorder requires simulation reader and explicit evidence endpoints")
	}
	reader, e := client.New(cfg.Reader)
	if e != nil {
		return e
	}
	var readers []*client.Client
	for _, endpoint := range cfg.EvidenceSources {
		g := endpoint.Grant.Grant
		if g.Domain != "simulation" || g.Tenant != grant.Tenant || g.Site != grant.Site {
			return errors.New("evidence endpoint outside recording scope")
		}
		c, e := client.New(endpoint)
		if e != nil {
			return e
		}
		readers = append(readers, c)
	}
	start := time.Now()
	end := start.Add(*duration)
	requestCtx, cancelRequests := context.WithDeadlineCause(ctx, end, errCaptureComplete)
	defer cancelRequests()
	completed := func(err error) bool {
		// net/http can return the deadline's cause instead of DeadlineExceeded.
		return ctx.Err() == nil && requestCtx.Err() == context.DeadlineExceeded && context.Cause(requestCtx) == errCaptureComplete && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errCaptureComplete))
	}
	frame := world.FrameRef{ID: *frameID, Revision: *revision}
	records := []world.Component{}
	// Reserve room for scope fields, timestamps and the manifest envelope.
	metadataBytes := 128 << 10
	seen := map[string]world.Component{}
	blobs := map[string][]byte{}
	total := int64(0)
	polls := 0

capture:
	for time.Now().Before(end) {
		var rawSnapshot json.RawMessage
		e := reader.Do(requestCtx, "read", http.MethodGet, "/v1/components", nil, &rawSnapshot)
		if e != nil {
			if completed(e) {
				break
			}
			return e
		}
		var snapshot struct {
			Components []json.RawMessage `json:"components"`
			Cursor     string            `json:"cursor"`
		}
		if e = authority.DecodeStrictLimit(rawSnapshot, &snapshot, 1<<20); e != nil {
			return e
		}
		if snapshot.Components == nil {
			return errors.New("snapshot components must be an array")
		}
		polls++
		for _, raw := range snapshot.Components {
			if err := requestCtx.Err(); err != nil {
				if completed(err) {
					break capture
				}
				return err
			}
			var c world.Component
			if e = authority.DecodeStrictLimit(raw, &c, 1<<20); e != nil {
				return e
			}
			if c.Key.Type != sensing.ComponentType {
				continue
			}
			c, e = recording.DecodeOriginal(raw)
			if e != nil {
				return e
			}
			// A replica of another run/frame is a configuration error, not a sample filter.
			s, e := recording.ValidateOriginal(c)
			if e != nil {
				return e
			}
			if c.Key.Tenant != grant.Tenant || c.Key.Site != grant.Site || c.Frame != frame || s.RunID != *runID {
				return errors.New("sensor outside original recording scope")
			}
			identity, _ := json.Marshal(struct {
				Key     world.Key
				Version world.Version
			}{c.Key, c.Version})
			key := string(identity)
			if old, ok := seen[key]; ok {
				if !recording.SameRecord(old, c) {
					return errors.New("conflicting duplicate sensor version")
				}
				continue
			}
			if len(records) >= recording.MaxRecords {
				return errors.New("recording record quota exceeded")
			}
			if metadataBytes+len(raw)+1 > recording.MaxManifestBytes {
				return errors.New("recording metadata quota exceeded")
			}
			now := time.Now()
			span := time.Duration(0)
			if s.Audio != nil {
				span = time.Duration(s.Audio.DurationNS)
			}
			expiry := c.Timing.SampleTime.Add(c.TTL - c.Timing.ErrorBound - span)
			if c.Timing.SampleTime.After(now) || !now.Before(expiry) || c.Timing.AgeHint+span >= c.TTL {
				return errors.New("sensor was stale or future at recording")
			}
			if s.Camera != nil && !now.Before(c.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS)-c.Timing.ErrorBound)) {
				return errors.New("camera calibration expired at recording")
			}
			// Keep a candidate's media private until the complete record is verified.
			var newBlob []byte
			if ref := s.Evidence; ref != nil {
				if _, exists := blobs[ref.SHA256]; !exists {
					var data []byte
					found := false
					if total+ref.Size > recording.MaxTotalBytes-recording.MaxManifestBytes {
						return errors.New("recording media quota exceeded")
					}
					for _, mediaReader := range readers {
						got, meta, err := mediaReader.DownloadEvidence(requestCtx, ref.SHA256, ref.Size)
						if err != nil && completed(err) {
							break capture
						}
						if ctx.Err() != nil {
							return ctx.Err()
						}
						if err == nil && int64(len(got)) == ref.Size && meta.Size == ref.Size && meta.ContentType == ref.ContentType && time.Now().UnixMilli() < meta.RetainUntilUnixMS && recording.Digest(got) == ref.SHA256 {
							data = got
							found = true
							break
						}
					}
					if !found {
						return errors.New("required recording media unavailable or invalid")
					}
					newBlob = data
				}
			}
			if err := requestCtx.Err(); err != nil {
				if completed(err) {
					break capture
				}
				return err
			}
			if newBlob != nil {
				blobs[s.Evidence.SHA256] = newBlob
				total += int64(len(newBlob))
			}
			records = append(records, c)
			metadataBytes += len(raw) + 1
			seen[key] = c
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			break
		}
		wait := min(*interval, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	bundle, e := recording.Build(recording.Manifest{Tenant: grant.Tenant, Site: grant.Site, RunID: *runID, Frame: frame, RecordedAt: start, CompletedAt: time.Now(), Records: records}, blobs)
	if e != nil {
		return e
	}
	if e = recording.Write(bundle, *out); e != nil {
		return e
	}
	return json.NewEncoder(output).Encode(map[string]any{"recording_sha256": bundle.SHA256, "records": len(bundle.Manifest.Records), "blobs": len(bundle.Blobs), "polls": polls, "path": *out, "capture_started_at": bundle.Manifest.StartedAt, "capture_ended_at": bundle.Manifest.EndedAt, "recorded_at": bundle.Manifest.RecordedAt, "completed_at": bundle.Manifest.CompletedAt, "integrity": recording.Integrity, "lossless": false})
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := record(ctx, os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
