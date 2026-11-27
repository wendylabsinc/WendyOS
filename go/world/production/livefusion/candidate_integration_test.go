package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func captureAt(t *testing.T, s *fixtureServer, sensor string, at time.Duration) []byte {
	return captureWithBounds(t, s, sensor, at, 0, 3*time.Second)
}

func captureWithBounds(t *testing.T, s *fixtureServer, sensor string, at, age, ttl time.Duration) []byte {
	t.Helper()
	_, original, err := decodeEnvelope(s.original[sensor])
	if err != nil {
		t.Fatal(err)
	}
	capture, err := sensorrecord.Decode(original.Publication.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := capture.Sample()
	if err != nil {
		t.Fatal(err)
	}
	sample.SampleNS = int64(at)
	capture.CapturedUnixNS -= int64(age)
	sample.Sequence += uint64(at / time.Millisecond)
	sample.ID = s.cfg.RunID + "/" + sensor + "/" + strconv.FormatUint(sample.Sequence, 10)
	capture.SampleJSON, err = json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if wire := capture.SensorLink; wire != nil {
		capture.SensorLink = nil
		capture, err = sensorrecord.BindSensorLink(capture, sensorrecord.SensorLinkFrame{
			ChannelID: wire.ChannelID, Seq: wire.FrameSequence, TsUs: wire.TimestampUS,
			Flags: wire.Flags, Payload: s.media[sensor+"/"+wire.Evidence.SHA256],
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := sensorrecord.Publication(capture, ttl)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.publishers[sensor].Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBufferedCaptureAuthority(t *testing.T) {
	for _, fault := range []string{"complete", "physical-expiry", "sample-expiry", "replacement", "forged"} {
		t.Run(fault, func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 2
			// Neither complete query is coherent. The first camera is old;
			// the second gas sample is too new for the original audio window.
			first := map[string][]byte{"camera-a": s.original["camera-a"]}
			for _, sensor := range []string{"camera-b", "gas-1", "audio-1"} {
				age, ttl := time.Duration(0), 3*time.Second
				if sensor == "gas-1" && fault == "physical-expiry" {
					age = 4900 * time.Millisecond
				}
				if sensor == "gas-1" && fault == "sample-expiry" {
					ttl = time.Second
				}
				first[sensor] = captureWithBounds(t, s, sensor, 650*time.Millisecond, age, ttl)
			}
			if fault == "forged" {
				var proof serviceorigin.SignedRecord
				if err := json.Unmarshal(first["gas-1"], &proof); err != nil {
					t.Fatal(err)
				}
				proof.Signature[0] ^= 1
				first["gas-1"], _ = json.Marshal(proof)
			}
			second := map[string][]byte{
				"camera-a": captureAt(t, s, "camera-a", 650*time.Millisecond),
				"camera-b": first["camera-b"],
				"gas-1":    captureAt(t, s, "gas-1", 850*time.Millisecond),
				"audio-1":  first["audio-1"],
			}
			if fault == "replacement" {
				s.faultQuery, s.faultKind = 3, "replacement"
			}
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				if n == 1 {
					s.proofs = first
				}
				if n == 2 {
					s.proofs = second
					s.sampleAdvance = 500 * time.Millisecond
					if fault == "physical-expiry" {
						s.clock.advance(200 * time.Millisecond)
					}
					if fault == "sample-expiry" {
						s.sampleAdvance = 1300 * time.Millisecond
					}
				}
				return nil
			}}
			out, err := Run(context.Background(), s.cfg, client)
			if fault != "complete" {
				if s.published != 0 || out.LocallyVerified || out.Attempts != 2 {
					t.Fatalf("buffered input bypassed %s: status=%s published=%d err=%v", fault, out.Status, s.published, err)
				}
				if fault == "sample-expiry" || fault == "physical-expiry" {
					if err != nil || out.Status != "insufficient_evidence" || out.Evaluation == nil {
						t.Fatal("expired buffered source did not become insufficient evidence", err, out.Status)
					}
				} else if err == nil || out.Status != "failed" {
					t.Fatal("buffered authority failure was ignored", err, out.Status)
				}
				if fault == "replacement" || fault == "forged" {
					if out.FailureStage != "verify_input" || out.ErrorCode != "signature_or_authority_refused" {
						t.Fatal("buffered proof failed before intended authority check", out.FailureStage, out.ErrorCode)
					}
				}
				foundOldGas := false
				for _, input := range out.Inputs {
					if input.Reference.Key.Source.Stream == "gas-1" {
						foundOldGas = bytes.Equal(input.EnvelopeJSON, first["gas-1"])
					}
				}
				if !foundOldGas {
					t.Fatal("refusal did not select the original buffered gas proof")
				}
				if out.Diagnostics == nil || len(out.Diagnostics.Attempts) != 2 {
					t.Fatal("buffered refusal omitted original discovery attempts")
				}
				attempt := out.Diagnostics.Attempts[1]
				if fault == "physical-expiry" && attempt.RetryReason != "physical_capture_expired" || fault == "sample-expiry" && attempt.RetryReason != "sample_capture_expired" {
					t.Fatal("original buffered expiry was not classified", attempt.RetryReason)
				}
				if attempt.Selection == nil || !attempt.Selection.Buffered || attempt.Selection.CaptureSpanNS != int64(250*time.Millisecond) || attempt.CaptureSpanNS != int64(450*time.Millisecond) || attempt.MatchingElapsedNS <= 0 {
					t.Fatal("refusal did not exercise the older coherent tuple")
				}
				return
			}
			if err != nil || out.Status != "complete" || !out.LocallyVerified || out.Attempts != 2 || s.published != 1 || len(out.Inputs) != 4 || len(out.Media) != 3 {
				t.Fatalf("coherent buffered originals did not publish: status=%s attempts=%d published=%d err=%v", out.Status, out.Attempts, s.published, err)
			}
			for _, input := range out.Inputs {
				stream := input.Reference.Key.Source.Stream
				want := second[stream]
				if stream == "gas-1" {
					want = first[stream]
				}
				if !bytes.Equal(input.EnvelopeJSON, want) {
					t.Fatal("selected proof bytes were changed or latest-only", stream)
				}
			}
		})
	}
}
