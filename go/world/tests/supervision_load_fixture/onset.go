package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

type onsetRelease struct {
	Schema  string `json:"schema"`
	RunID   string `json:"run_id"`
	Variant string `json:"variant"`
	Release int64  `json:"release_unix_ms"`
	End     int64  `json:"window_end_unix_ms"`
	Publish bool   `json:"publish"`
}
type onsetReport struct {
	Schema             string              `json:"schema"`
	Domain             string              `json:"domain"`
	RunID              string              `json:"run_id"`
	Variant            string              `json:"variant"`
	Stage              string              `json:"stage"`
	Ready              int64               `json:"ready_at_unix_ms"`
	Observed           int64               `json:"release_observed_at_unix_ms"`
	Release            int64               `json:"release_unix_ms"`
	End                int64               `json:"window_end_unix_ms"`
	Started            int64               `json:"started_at_unix_ms"`
	Finished           int64               `json:"finished_at_unix_ms"`
	FirstSend          int64               `json:"first_request_attempt_at_unix_ms"`
	FirstCompletion    int64               `json:"first_completion_at_unix_ms"`
	FirstACK           int64               `json:"first_ack_at_unix_ms"`
	Epoch              uint64              `json:"epoch"`
	FirstSequence      uint64              `json:"first_sequence"`
	Attempted          uint64              `json:"attempted"`
	Published          uint64              `json:"published"`
	Elapsed            int64               `json:"elapsed_ns"`
	Rate               float64             `json:"achieved_publications_per_second"`
	PublicationEnabled bool                `json:"publication_enabled"`
	ErrorClass         string              `json:"error_class"`
	Failures           map[string]uint64   `json:"failures"`
	Metrics            runtimeobs.Snapshot `json:"request_metrics"`
}

func writeOnset(path string, report onsetReport, first bool) error {
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("onset report bound")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".onset-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if first {
		return os.Link(f.Name(), path)
	}
	return os.Rename(f.Name(), path)
}
func validateOnsetRelease(r onsetRelease, o options, now time.Time) error {
	if r.Schema != "wendy.world.publication-onset-release.v1" || r.RunID != o.runID || r.Variant != o.onsetVariant || r.Release <= 0 || r.End-r.Release < time.Second.Milliseconds() || r.End-r.Release > 24000 || abs(r.Release-now.UnixMilli()) > 2000 || (o.onsetVariant == "fixed" && r.Publish) {
		return errors.New("onset release refused")
	}
	return nil
}
func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func runOnset(parent context.Context, o options, output io.Writer) (resultErr error) {
	if (o.onsetVariant != "fixed" && o.onsetVariant != "changing") || o.preload || o.verifyPreload || o.entities != 200 || o.hz != 2 || o.duration != 24*time.Second {
		return errors.New("onset mode refused")
	}
	paths := map[string]bool{}
	for _, p := range []string{o.credentials, o.trust, o.manifest, o.release, o.report} {
		if !filepath.IsAbs(p) || paths[filepath.Clean(p)] {
			return errors.New("onset paths refused")
		}
		paths[filepath.Clean(p)] = true
	}
	cfg, c, err := load(o)
	if err != nil {
		return err
	}
	defer clear(cfg.PrivateKey)
	var manifest preloadManifest
	if readPrivate(o.manifest, &manifest) != nil || manifest.Schema != "wendy.world.preload-manifest.v1" || manifest.RunID != o.runID || manifest.Count != 200 || manifest.Epoch == 0 || len(manifest.Digests) != 200 {
		return errors.New("onset manifest refused")
	}
	for _, d := range manifest.Digests {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(d) {
			return errors.New("onset digest refused")
		}
	}
	metrics := runtimeobs.New()
	r := onsetReport{Schema: "wendy.world.publication-onset-publisher.v1", Domain: "simulation", RunID: o.runID, Variant: o.onsetVariant, Stage: "ready", Ready: time.Now().UnixMilli(), Epoch: manifest.Epoch, FirstSequence: 201,
		Failures: map[string]uint64{"timeout_or_cancel": 0, "authority": 0, "conflict": 0, "rate_limit": 0, "other_http": 0, "transport_or_response": 0}}
	if err = writeOnset(o.report, r, true); err != nil {
		return errors.New("onset report refused")
	}
	defer func() {
		r.Finished = time.Now().UnixMilli()
		r.Metrics = metrics.Snapshot()
		if resultErr != nil {
			r.Stage = "failed"
		} else {
			r.Stage = "finished"
		}
		_ = writeOnset(o.report, r, false)
		if json.NewEncoder(output).Encode(r) != nil && resultErr == nil {
			resultErr = errors.New("onset output failed")
		}
	}()
	wait, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var release onsetRelease
	for {
		if wait.Err() != nil {
			r.ErrorClass = "gate_timeout_or_cancel"
			return errors.New("onset gate expired")
		}
		err = readPrivate(o.release, &release)
		if err == nil {
			break
		}
		// readPrivate deliberately normalizes unsafe input errors. Only absence waits.
		if _, statErr := os.Lstat(o.release); !os.IsNotExist(statErr) {
			r.ErrorClass = "release_refused"
			return errors.New("onset release refused")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	now := time.Now()
	r.Observed = now.UnixMilli()
	if validateOnsetRelease(release, o, now) != nil {
		r.ErrorClass = "release_refused"
		return errors.New("onset release refused")
	}
	r.Release = release.Release
	r.End = release.End
	r.PublicationEnabled = release.Publish
	if delay := time.Until(time.UnixMilli(release.Release)); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-wait.Done():
			timer.Stop()
			r.ErrorClass = "gate_cancel"
			return errors.New("onset gate cancelled")
		case <-timer.C:
		}
	}
	if wait.Err() != nil || time.Now().UnixMilli() < release.Release {
		r.ErrorClass = "gate_clock_or_cancel"
		return errors.New("onset release unavailable")
	}
	remaining := time.Until(time.UnixMilli(release.End))
	if remaining <= 0 || remaining > 24*time.Second {
		r.ErrorClass = "gate_elapsed"
		return errors.New("onset window elapsed")
	}
	ctx, end := context.WithTimeout(parent, remaining)
	defer end()
	started := time.Now()
	r.Started = started.UnixMilli()
	r.Stage = "released"
	_ = writeOnset(o.report, r, false)
	if !release.Publish {
		<-ctx.Done()
		r.Elapsed = time.Since(started).Nanoseconds()
		return nil
	}
	interval := time.Second / 400
	for ctx.Err() == nil {
		tick := time.Now()
		r.Attempted++
		record := component(cfg, o.runID, manifest.Epoch, 200+r.Attempted, int((r.Attempted-1)%200), tick)
		record.TTL = preloadTTL
		if r.FirstSend == 0 {
			r.FirstSend = tick.UnixMilli()
		}
		request, done := context.WithTimeout(ctx, time.Second)
		err = c.Publish(request, record)
		done()
		metrics.Observe(runtimeobs.HTTPOther, time.Since(tick))
		finished := time.Now().UnixMilli()
		first := r.FirstCompletion == 0
		if first {
			r.FirstCompletion = finished
		}
		if err == nil {
			r.Published++
			if r.FirstACK == 0 {
				r.FirstACK = finished
				first = true
			}
		} else {
			metrics.Increment(runtimeobs.HTTPFailures)
			r.Failures[failureClass(err)]++
		}
		if first {
			_ = writeOnset(o.report, r, false)
		}
		if pause := interval - time.Since(tick); pause > 0 {
			timer := time.NewTimer(pause)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	r.Elapsed = time.Since(started).Nanoseconds()
	r.Rate = float64(r.Published) / time.Since(started).Seconds()
	return nil
}
