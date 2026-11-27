package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/recording"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func server(t *testing.T, mode string) (string, string) {
	return blockingServer(t, mode, nil)
}

func blockingServer(t *testing.T, mode string, blocked chan<- struct{}) (string, string) {
	t.Helper()
	media := []byte("archived WAV bytes")
	secondMedia := []byte("second WAV requiring another chunk")
	at := time.Now().Add(-time.Second)
	sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/audio-1/1", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, SensorID: "audio-1", Kind: "audio", Sequence: 1, SampleNS: int64(500 * time.Millisecond), RunID: "run", ScenarioVersion: "v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "audio", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "v1", Audio: &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(100 * time.Millisecond)}, Evidence: &sensing.EvidenceRef{SHA256: recording.Digest(media), Size: int64(len(media)), ContentType: "audio/wav"}}
	conv, e := sensing.New(sensing.Config{Tenant: "tenant", Site: "office", RunID: "run", Frame: sample.Frame, Source: world.Source{Device: "node", App: "sensor", Service: "audio-1", Stream: "samples"}, Epoch: 2, Anchor: at, TTL: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(sample)
	first, e := conv.Convert(raw)
	if e != nil {
		t.Fatal(e)
	}
	sample.Sequence = 2
	sample.ID = "run/audio-1/2"
	sample.SampleNS += int64(10 * time.Millisecond)
	if mode == "blockmedia" {
		sample.Evidence = &sensing.EvidenceRef{SHA256: recording.Digest(secondMedia), Size: int64(len(secondMedia)), ContentType: "audio/wav"}
	}
	raw, _ = json.Marshal(sample)
	second, e := conv.Convert(raw)
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int64
	retain := time.Now().Add(time.Minute).UnixMilli()
	block := func(r *http.Request) {
		if blocked != nil {
			select {
			case blocked <- struct{}{}:
			default:
			}
		}
		<-r.Context().Done()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/components":
			if mode == "missingcomponents" {
				w.Write([]byte(`{}`))
				return
			}
			if mode == "nullcomponents" {
				w.Write([]byte(`{"components":null}`))
				return
			}
			n := calls.Add(1)
			if mode == "blockpoll" && n >= 2 {
				block(r)
				return
			}
			if mode == "pollerror" && n >= 2 {
				http.Error(w, "failed next poll", http.StatusServiceUnavailable)
				return
			}
			c := first
			if n >= 2 {
				c = second
			}
			if mode == "conflict" && n >= 2 {
				c = first
				c.TTL += time.Millisecond
			}
			if mode == "scope" {
				c.Key.Site = "wrong"
			}
			if mode == "stale" {
				c.Timing.SampleTime = time.Now().Add(-time.Hour)
			}
			if mode == "future" {
				c.Timing.SampleTime = time.Now().Add(time.Hour)
			}
			wire, _ := json.Marshal(client.Snapshot{Components: []world.Component{c}})
			if mode == "missingclock" {
				wire = bytes.Replace(wire, []byte(`"error_bound_ns":0,`), nil, 1)
			}
			if mode == "nullclock" {
				wire = bytes.Replace(wire, []byte(`"error_bound_ns":0`), []byte(`"error_bound_ns":null`), 1)
			}
			if mode == "missingseed" {
				wire = bytes.Replace(wire, []byte(`"seed":0,`), nil, 1)
			}
			w.Write(wire)
		case "/v1/evidence/get":
			var request evidence.GetRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if mode == "blockmedia" && request.SHA256 == recording.Digest(secondMedia) {
				if request.Offset > 0 {
					block(r)
					return
				}
				json.NewEncoder(w).Encode(evidence.GetResponse{SHA256: request.SHA256, Size: int64(len(secondMedia)), ContentType: "audio/wav", RetainUntilUnixMS: retain, Data: secondMedia[:2]})
				return
			}
			if mode == "missing" {
				http.Error(w, "missing", 404)
				return
			}
			data := append([]byte(nil), media...)
			if mode == "corrupt" {
				data[0] = 'x'
			}
			json.NewEncoder(w).Encode(evidence.GetResponse{SHA256: recording.Digest(media), Size: int64(len(media)), ContentType: "audio/wav", RetainUntilUnixMS: retain, Data: data})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	cfg := client.Config{URL: srv.URL, Audience: "node", AllowInsecureSimulation: true, PrivateKey: key, Grant: authority.SignedGrant{Grant: authority.Grant{PublicKey: pub, Domain: "simulation", Tenant: "tenant", Site: "office"}}}
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	data, _ := json.Marshal(config{Reader: cfg, EvidenceSources: []client.Config{cfg}})
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	return path, filepath.Join(root, "archive")
}
func args(path, out string) []string {
	return []string{"--config", path, "--out", out, "--run-id", "run", "--frame", "floor", "--frame-revision", "v1", "--duration", "130ms", "--interval", "20ms"}
}
func TestRecorderCapturesHistoryAndVerifiesMedia(t *testing.T) {
	path, out := server(t, "")
	var output bytes.Buffer
	if e := record(context.Background(), args(path, out), &output); e != nil {
		t.Fatal(e)
	}
	b, e := recording.Load(out)
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Manifest.Records) != 2 || len(b.Blobs) != 1 || b.Manifest.Records[0].Version.Sequence != 1 || b.Manifest.Records[1].Version.Sequence != 2 {
		t.Fatal("history not preserved")
	}
	if !bytes.Contains(output.Bytes(), []byte(`"lossless":false`)) {
		t.Fatal(output.String())
	}
	if e = record(context.Background(), args(path, out), &output); e == nil {
		t.Fatal("overwrote output")
	}
}
func TestRecorderFailsWithoutPartialInstallation(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "scope", "stale", "future", "conflict", "missingclock", "nullclock", "missingseed", "missingcomponents", "nullcomponents", "pollerror"} {
		t.Run(mode, func(t *testing.T) {
			path, out := server(t, mode)
			if e := record(context.Background(), args(path, out), &bytes.Buffer{}); e == nil {
				t.Fatal("accepted " + mode)
			}
			if _, e := os.Lstat(out); !os.IsNotExist(e) {
				t.Fatal("partial install exists")
			}
		})
	}
}

func TestRecorderDurationCompletesDuringRequest(t *testing.T) {
	for _, mode := range []string{"blockpoll", "blockmedia"} {
		t.Run(mode, func(t *testing.T) {
			blocked := make(chan struct{}, 1)
			path, out := blockingServer(t, mode, blocked)
			var output bytes.Buffer
			if err := record(context.Background(), append(args(path, out), "--duration", "400ms"), &output); err != nil {
				t.Fatal(err)
			}
			select {
			case <-blocked:
			default:
				t.Fatal("request did not reach the controlled blocking point")
			}
			bundle, err := recording.Load(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Manifest.Records) != 1 || bundle.Manifest.Records[0].Version.Sequence != 1 || len(bundle.Blobs) != 1 {
				t.Fatal("interrupted candidate or partial media entered the archive")
			}
			if !bytes.Contains(output.Bytes(), []byte(`"records":1`)) {
				t.Fatal(output.String())
			}
		})
	}
}

func TestRecorderCallerCancellationDuringRequestDoesNotFinalize(t *testing.T) {
	for _, mode := range []string{"blockpoll", "blockmedia"} {
		t.Run(mode, func(t *testing.T) {
			blocked := make(chan struct{}, 1)
			path, out := blockingServer(t, mode, blocked)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- record(ctx, append(args(path, out), "--duration", "2s"), &bytes.Buffer{})
			}()
			select {
			case <-blocked:
				cancel()
			case <-time.After(3 * time.Second):
				t.Fatal("request did not reach the controlled blocking point")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation became success or another error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("recorder did not stop after caller cancellation")
			}
			if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatal("caller cancellation installed an archive")
			}
		})
	}
}

func TestRecorderCallerDeadlineDoesNotCompleteDuration(t *testing.T) {
	path, out := server(t, "blockpoll")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := record(ctx, append(args(path, out), "--duration", "2s"), &bytes.Buffer{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline became success or another error: %v", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatal("caller deadline installed an archive")
	}
}
func TestRecorderRefusesPublicCredentialsAndCancellation(t *testing.T) {
	path, out := server(t, "")
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if e := record(context.Background(), args(path, out), &bytes.Buffer{}); e == nil {
		t.Fatal("accepted public config")
	}
	if e := os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := record(ctx, args(path, out), &bytes.Buffer{}); e == nil {
		t.Fatal("ignored cancellation")
	}
	if _, e := os.Lstat(out); !os.IsNotExist(e) {
		t.Fatal("cancelled archive exists")
	}
}
