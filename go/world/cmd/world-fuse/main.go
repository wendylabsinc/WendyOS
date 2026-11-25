// world-fuse runs the synthetic multimodal hazard rule as an ordinary app.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/appsupport"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type config struct {
	Publisher       client.Config           `json:"publisher"`
	EvidenceSources []client.Config         `json:"evidence_sources"`
	Sources         map[string]world.Source `json:"sources"`
	RequiredSensors map[string]string       `json:"required_sensors"`
}

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("world-fuse", flag.ContinueOnError)
	path := f.String("config", "", "private fusion app configuration")
	runID := f.String("run-id", "", "simulation run")
	duration := f.Duration("duration", 0, "optional bounded run duration")
	replayMode := f.Bool("replay", false, "evaluate recorded inputs only")
	recordingDigest := f.String("recording-sha256", "", "exact recording digest required in replay mode")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *runID == "" || f.NArg() != 0 {
		return errors.New("run-id required")
	}
	if (*replayMode && !sensing.DigestValid(*recordingDigest)) || (!*replayMode && *recordingDigest != "") {
		return errors.New("--replay and an exact --recording-sha256 must be supplied together")
	}
	var cfg config
	if err := appsupport.ReadJSON(*path, &cfg); err != nil {
		return err
	}
	if cfg.Publisher.Grant.Grant.Domain != "simulation" || len(cfg.EvidenceSources) < 1 || len(cfg.EvidenceSources) > 8 || len(cfg.RequiredSensors) != 4 || len(cfg.Sources) != len(cfg.RequiredSensors) {
		return errors.New("invalid simulation fusion configuration")
	}
	publisher, err := client.New(cfg.Publisher)
	if err != nil {
		return err
	}
	var readers []*client.Client
	for _, source := range cfg.EvidenceSources {
		if source.Grant.Grant.Domain != "simulation" || source.Grant.Grant.Site != cfg.Publisher.Grant.Grant.Site || source.Grant.Grant.Tenant != cfg.Publisher.Grant.Grant.Tenant {
			return errors.New("evidence source outside fusion scope")
		}
		c, err := client.New(source)
		if err != nil {
			return err
		}
		readers = append(readers, c)
	}
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	epoch, err := awaitEpoch(ctx, publisher)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	publications := publicationState{publisher: publisher, epoch: epoch}
	lastState, lastAvailability := "", ""
	availability := func(state string) {
		if state != lastAvailability {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"availability": state})
			lastAvailability = state
		}
	}
	var loadTerminal error
	loader := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		var terminal error
		for _, reader := range readers {
			data, meta, err := reader.DownloadEvidence(ctx, ref.SHA256, ref.Size)
			if err == nil && meta.Size == ref.Size && meta.ContentType == ref.ContentType && time.Now().UnixMilli() < meta.RetainUntilUnixMS {
				return data, nil
			}
			var status *client.HTTPStatusError
			if errors.As(err, &status) && (status.StatusCode == 401 || status.StatusCode == 403) {
				terminal = err
			}
		}
		loadTerminal = terminal
		return nil, errors.New("evidence unavailable at configured capture nodes")
	}
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	var retry retryDelay
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !retry.ready(time.Now()) {
			continue
		}
		tick, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := publisher.Query(tick)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if !client.Transient(err) {
				return err
			}
			availability("world unavailable")
			retry.failed(time.Now())
			continue
		}
		loadTerminal = nil
		result, err := fusion.Evaluate(tick, fusion.Config{Tenant: cfg.Publisher.Grant.Grant.Tenant, Site: cfg.Publisher.Grant.Grant.Site, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, ClockDomain: "simulation:" + *runID, RunID: *runID, Now: time.Now, RequiredSensors: cfg.RequiredSensors, Sources: cfg.Sources, Replay: *replayMode, RecordingSHA256: *recordingDigest}, snapshot.Components, loader)
		if loadTerminal != nil {
			cancel()
			return loadTerminal
		}
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if !client.Transient(err) {
				return err
			}
			availability("evaluation unavailable")
			retry.failed(time.Now())
			continue
		}
		if result.State != lastState {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"state": result.State, "reason": result.Reason})
			lastState = result.State
		}
		var desired *world.Component
		if result.Hazard != nil && time.Now().Before(result.Hazard.ValidUntil) {
			component, err := result.Component(cfg.Publisher.Grant.Grant.Source, epoch, 1)
			if err != nil && time.Now().Before(result.Hazard.ValidUntil) {
				cancel()
				return err
			}
			if err == nil {
				desired = &component
			}
		}
		if err := publications.update(time.Now(), desired, result.InputDigest); err != nil {
			cancel()
			return err
		}
		published, err := publications.send(tick, time.Now())
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !client.Transient(err) {
				return err
			}
			availability("publication unavailable")
			retry.failed(time.Now())
			continue
		}
		retry.reset()
		availability("available")
		if published != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"published": true, "component": published})
		}
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
