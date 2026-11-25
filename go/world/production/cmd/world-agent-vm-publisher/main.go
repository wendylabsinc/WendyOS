// world-agent-vm-publisher exercises the ordinary app API in an isolated VM.
// Its observations are synthetic fixtures, never measurements from hardware.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const clockDomain = "simulation:agent-vm-fixture"

type receipt struct {
	Mode          string `json:"mode"`
	Proof         string `json:"proof"`
	SHA256        string `json:"sha256"`
	PayloadSHA256 string `json:"payload_sha256"`
}

func request(stream string) *appv1.PublishRequest {
	payload := []byte(`{ "value": 12.5, "unit": "ppm", "sensor_type": "synthetic-gas", "calibration_revision": "vm-fixture-1", "uncertainty": 0.5 }`)
	if stream == "temperature" {
		payload = []byte(`{ "value": 21.0, "unit": "degC", "sensor_type": "synthetic-temperature", "calibration_revision": "vm-fixture-1", "uncertainty": 0.2 }`)
	}
	return &appv1.PublishRequest{
		Entity: "fixture-" + stream, ComponentType: "wendy.sensor.scalar.v1", Stream: stream,
		Frame:       &appv1.FrameRef{Id: "office-floor", Revision: "vm-fixture-1"},
		Timing:      &appv1.Timing{SampleUnixNs: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(), ClockDomain: clockDomain, ErrorKnown: true},
		TtlNs:       uint64(30 * time.Second),
		Provenance:  &appv1.Provenance{Origin: appv1.Origin_ORIGIN_SIMULATED, AppVersion: "1.0.0", Simulation: &appv1.SimulationContext{RunId: "agent-vm-fixture", ScenarioVersion: "private-agent-v1", Seed: 1, ClockDomain: clockDomain}},
		PayloadJson: payload,
	}
}

func save(directory, mode string, req *appv1.PublishRequest, reply *appv1.PublishReply) (receipt, error) {
	if reply == nil || !reply.Accepted || len(reply.SignedRecordJson) == 0 || len(reply.SignedRecordJson) > 256<<10 || !json.Valid(reply.SignedRecordJson) {
		return receipt{}, errors.New("missing bounded accepted original proof")
	}
	name := mode + ".json"
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return receipt{}, err
	}
	_, err = f.Write(reply.SignedRecordJson)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return receipt{}, err
	}
	if closeErr != nil {
		return receipt{}, closeErr
	}
	original, err := protojson.Marshal(req)
	if err != nil {
		return receipt{}, err
	}
	if err = os.WriteFile(filepath.Join(directory, mode+".request.json"), original, 0600); err != nil {
		return receipt{}, err
	}
	h := sha256.Sum256(reply.SignedRecordJson)
	payloadHash := sha256.Sum256(req.PayloadJson)
	return receipt{mode, name, hex.EncodeToString(h[:]), hex.EncodeToString(payloadHash[:])}, nil
}

func run(ctx context.Context) error {
	stream := os.Getenv("FIXTURE_STREAM")
	if stream != "gas" && stream != "temperature" {
		return errors.New("FIXTURE_STREAM must be gas or temperature")
	}
	socket := os.Getenv("WENDY_WORLD_SOCKET")
	if socket != "/run/wendy/world/world.sock" {
		return errors.New("expected the agent-mounted service socket")
	}
	directory, err := os.MkdirTemp("/proofs", "run-")
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<10)))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := appv1.NewWorldAppServiceClient(conn)
	trial, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req := request(stream)
	reply, err := client.Publish(trial, req)
	if err != nil {
		return fmt.Errorf("unary publication: %w", err)
	}
	one, err := save(directory, "unary", req, reply)
	if err != nil {
		return err
	}
	bidi, err := client.PublishStream(trial)
	if err != nil {
		return err
	}
	if err = bidi.Send(req); err != nil {
		return err
	}
	reply, err = bidi.Recv()
	if err != nil {
		return err
	}
	two, err := save(directory, "stream", req, reply)
	if err != nil {
		return err
	}
	if err = bidi.CloseSend(); err != nil {
		return err
	}
	other := "gas"
	if stream == "gas" {
		other = "temperature"
	}
	bad := proto.Clone(req).(*appv1.PublishRequest)
	bad.Stream = other
	if _, err = client.Publish(trial, bad); status.Code(err) != codes.PermissionDenied {
		return fmt.Errorf("sibling stream refusal: got %v", status.Code(err))
	}
	bad = proto.Clone(req).(*appv1.PublishRequest)
	bad.Provenance = &appv1.Provenance{Origin: appv1.Origin_ORIGIN_MEASURED, AppVersion: "1.0.0"}
	if _, err = client.Publish(trial, bad); status.Code(err) != codes.PermissionDenied {
		return fmt.Errorf("physical provenance refusal: got %v", status.Code(err))
	}
	bad = proto.Clone(req).(*appv1.PublishRequest)
	bad.Frame.Revision = "uncommissioned"
	if _, err = client.Publish(trial, bad); status.Code(err) != codes.Unavailable {
		return fmt.Errorf("frame admission refusal: got %v", status.Code(err))
	}
	// A rejected request must not disable another valid request from the service.
	reply, err = client.Publish(trial, req)
	if err != nil {
		return err
	}
	three, err := save(directory, "after-refusals", req, reply)
	if err != nil {
		return err
	}
	summary := struct {
		Service   string    `json:"service"`
		Directory string    `json:"directory"`
		Proofs    []receipt `json:"proofs"`
		Refusals  []string  `json:"refusals"`
	}{stream, directory, []receipt{one, two, three}, []string{"sibling_stream", "physical_provenance", "uncommissioned_frame"}}
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, "summary.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		return err
	}
	// The VM runner may request one bounded publication after an agent restart.
	// It writes the marker in this fixture's ordinary output volume. No agent
	// credential or production control API is added for the probe.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			marker := filepath.Join(directory, "probe-request")
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
			if err := os.Rename(marker, filepath.Join(directory, "probe-consumed")); err != nil {
				return err
			}
			probeCtx, done := context.WithTimeout(ctx, 3*time.Second)
			got, probeErr := client.Publish(probeCtx, req)
			done()
			result := struct {
				Accepted bool   `json:"accepted"`
				Code     string `json:"code"`
			}{got != nil && got.Accepted, status.Code(probeErr).String()}
			data, err := json.Marshal(result)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, "probe-result.json"), data, 0600); err != nil {
				return err
			}
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
