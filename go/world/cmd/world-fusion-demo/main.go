// world-fusion-demo evaluates the shared fusion core over local simulator
// samples. It is a persistent NDJSON process and performs no signing or network
// operation. Sample timestamps are elapsed nanoseconds from 2026-01-01 UTC.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/fusiondemo"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const maxLine = 16 << 20

type request struct {
	RunID   string            `json:"run_id"`
	NowNS   int64             `json:"now_ns"`
	Frame   world.FrameRef    `json:"frame"`
	Samples []json.RawMessage `json:"samples"`
	Media   map[string][]byte `json:"media"`
}
type evaluator struct {
	run        string
	frame      world.FrameRef
	lastNow    int64
	converters map[string]*sensing.Converter
}

func (e *evaluator) evaluate(raw []byte) (fusion.Result, error) {
	var req request
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return fusion.Result{}, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fusion.Result{}, errors.New("one request object required")
	}
	if req.NowNS < 0 || req.NowNS > int64(24*time.Hour) || len(req.Samples) < 4 || len(req.Samples) > 16 || req.Frame.Validate() != nil {
		return fusion.Result{}, errors.New("invalid demo batch")
	}
	if e.run != "" && (req.RunID != e.run || req.Frame != e.frame || req.NowNS < e.lastNow) {
		return fusion.Result{}, errors.New("run, frame or simulation clock changed")
	}
	samples := make([]sensing.Sample, len(req.Samples))
	required := map[string]string{}
	for i, raw := range req.Samples {
		s, err := sensing.Decode(raw)
		if err != nil {
			return fusion.Result{}, err
		}
		if s.SiteID != "studio-01" || s.RunID != req.RunID || s.Frame != req.Frame || required[s.SensorID] != "" {
			return fusion.Result{}, errors.New("sample scope or duplicate differs")
		}
		samples[i] = s
		required[s.SensorID] = s.Kind
	}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if e.converters == nil {
		e.converters = map[string]*sensing.Converter{}
	}
	components := make([]world.Component, 0, len(samples))
	for i, s := range samples {
		converter := e.converters[s.SensorID]
		if converter == nil {
			var err error
			converter, err = sensing.New(sensing.Config{Tenant: "simulation", Site: "studio-01", RunID: req.RunID, Frame: req.Frame, Source: world.Source{Device: "mujoco-demo", App: "world-fusion-demo", Service: s.SensorID, Stream: "samples"}, Epoch: 1, Anchor: anchor, TTL: 2 * time.Second})
			if err != nil {
				return fusion.Result{}, err
			}
			e.converters[s.SensorID] = converter
		}
		component, err := converter.Convert(req.Samples[i])
		if err != nil {
			return fusion.Result{}, err
		}
		components = append(components, component)
	}
	e.run, e.frame, e.lastNow = req.RunID, req.Frame, req.NowNS
	return fusion.Evaluate(context.Background(), fusion.Config{Tenant: "simulation", Site: "studio-01", RunID: req.RunID, Frame: req.Frame, ClockDomain: "simulation:" + req.RunID, RequiredSensors: required, Now: func() time.Time { return anchor.Add(time.Duration(req.NowNS)) }}, components, func(_ context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		data, ok := req.Media[ref.SHA256]
		if !ok {
			return nil, errors.New("media missing")
		}
		return data, nil
	})
}
func serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), maxLine)
	encoder := json.NewEncoder(output)
	e := new(evaluator)
	for scanner.Scan() {
		result, err := e.evaluate(scanner.Bytes())
		if err != nil {
			result = fusion.Result{State: fusion.InsufficientEvidence, Reason: "demo input refused: " + err.Error()}
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	configPath := flag.String("config", "", "fixed participant sensor catalog JSON")
	stateDir := flag.String("state-dir", "", "participant-local durable replica and media directory")
	flag.Parse()
	var err error
	if *configPath == "" && *stateDir == "" {
		err = serve(os.Stdin, os.Stdout)
	} else if *configPath == "" || *stateDir == "" || flag.NArg() != 0 {
		err = errors.New("config and state-dir must be supplied together")
	} else {
		var raw []byte
		raw, err = os.ReadFile(*configPath)
		if err == nil && len(raw) > 32<<10 {
			err = errors.New("participant configuration exceeds bound")
		}
		var cfg fusiondemo.Config
		if err == nil {
			err = decodeOne(raw, &cfg)
		}
		if err == nil {
			err = serveParticipant(os.Stdin, os.Stdout, cfg, *stateDir)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func decodeOne(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("one JSON object required")
	}
	return nil
}
func serveParticipant(input io.Reader, output io.Writer, cfg fusiondemo.Config, stateDir string) error {
	e, err := fusiondemo.New(cfg, stateDir)
	if err != nil {
		return err
	}
	defer e.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), fusiondemo.MaxLineBytes)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var tick fusiondemo.Tick
		err := decodeOne(scanner.Bytes(), &tick)
		out := fusiondemo.Output{ParticipantID: cfg.ParticipantID, NowNS: tick.NowNS}
		if err == nil {
			out, err = e.Tick(context.Background(), tick)
		}
		if err != nil {
			out.Result = fusion.Result{State: fusion.InsufficientEvidence, Reason: "participant input refused: " + err.Error()}
		}
		if err := encoder.Encode(out); err != nil {
			return err
		}
	}
	return scanner.Err()
}
