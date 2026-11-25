package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func main() {
	credentials := flag.String("credentials", "", "private assignee client configuration")
	adapterURL := flag.String("adapter", "", "isolated simulation actuator HTTP URL")
	tokenFile := flag.String("adapter-token-file", "", "private adapter token")
	runID := flag.String("run-id", "", "enable live inspection for this explicit simulation run")
	frame := flag.String("frame", "office-floor", "inspection site frame")
	revision := flag.String("revision", "office-frame-v1", "inspection frame revision")
	planarPath := flag.String("planar-config", "", "opt-in shared-office robot, floor and grid JSON")
	readTokenFile := flag.String("adapter-read-token-file", "", "separate shared-office read token, required for planar mode")
	flag.Parse()
	if *credentials == "" || *adapterURL == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "credentials, adapter and adapter-token-file are required")
		os.Exit(1)
	}
	f, err := os.Open(*credentials)
	if err != nil {
		fail(err)
	}
	var cfg client.Config
	d := json.NewDecoder(io.LimitReader(f, 65536))
	d.DisallowUnknownFields()
	err = d.Decode(&cfg)
	_ = f.Close()
	if err != nil {
		fail(err)
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		fail(fmt.Errorf("executor is restricted to simulation"))
	}
	c, err := client.New(cfg)
	if err != nil {
		fail(err)
	}
	f, err = os.Open(*tokenFile)
	if err != nil {
		fail(err)
	}
	token, err := io.ReadAll(io.LimitReader(f, 4097))
	_ = f.Close()
	if err != nil || len(token) > 4096 {
		fail(fmt.Errorf("invalid adapter token file"))
	}
	controller := executor.Controller{Gateway: executor.NodeGateway{Client: c}}
	var planarCapture *livecapture.PlanarConfig
	captureToken := strings.TrimSpace(string(token))
	if *planarPath != "" {
		if *runID == "" || *readTokenFile == "" {
			fail(fmt.Errorf("planar inspection requires run-id and adapter-read-token-file"))
		}
		f, err = os.Open(*readTokenFile)
		if err != nil {
			fail(err)
		}
		readToken, e := io.ReadAll(io.LimitReader(f, 4097))
		_ = f.Close()
		if e != nil || len(readToken) > 4096 {
			fail(fmt.Errorf("invalid adapter read token file"))
		}
		captureToken = strings.TrimSpace(string(readToken))
		planar, e := readPlanarConfig(*planarPath)
		if e != nil {
			fail(e)
		}
		controller.Planar, err = executor.NewPlanarNavigation(planar)
		if err != nil {
			fail(err)
		}
		controller.Adapter, err = executor.NewPlanarHTTPAdapter(*adapterURL, captureToken, strings.TrimSpace(string(token)), planar.RobotID, planar.FloorY)
		planarCapture = &livecapture.PlanarConfig{RobotID: planar.RobotID, FloorY: planar.FloorY}
	} else {
		controller.Adapter, err = executor.NewHTTPAdapter(*adapterURL, strings.TrimSpace(string(token)))
	}
	if err != nil {
		fail(err)
	}
	if *runID != "" {
		capture, err := livecapture.New(livecapture.Config{URL: *adapterURL, Token: captureToken, RunID: *runID, Site: cfg.Grant.Grant.Site, Frame: world.FrameRef{ID: *frame, Revision: *revision}, Planar: planarCapture})
		if err != nil {
			fail(err)
		}
		controller.Capture = capture
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	last := ""
	err = controller.Run(ctx, func(d tasking.Decision, e error) {
		state := fmt.Sprintf("%s/%s/%d/%v", d.Record.Spec.ID, d.Action, d.Record.Status.Version, e)
		if state == last {
			return
		}
		last = state
		event := map[string]any{"task_id": d.Record.Spec.ID, "action": d.Action, "status": d.Record.Status}
		if e != nil {
			event["error"] = e.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(event)
	})
	if err != nil {
		fail(err)
	}
}

func readPlanarConfig(path string) (executor.PlanarConfig, error) {
	var config executor.PlanarConfig
	f, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (512<<10)+1))
	if err != nil {
		return config, err
	}
	err = authority.DecodeStrictLimit(raw, &config, 512<<10)
	return config, err
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
