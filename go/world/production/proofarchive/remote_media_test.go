package proofarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestRemoteMediaEnvelopesPreserveProofsReferencesAndOriginalDeadlines(t *testing.T) {
	for _, kind := range []string{"camera", "audio", "sensorlink"} {
		t.Run(kind, func(t *testing.T) {
			r := remoteSetup(t)
			ctx := context.Background()
			raw := r.raw(t, kind)
			// The archive receives only the signed capture envelope. No image or
			// audio bytes are passed to it, and no media reader is configured.
			ref, err := r.e.a.RememberRemoteAccepted(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Key.Source.Device != r.id.Principal || ref.Key.Source.Device == r.e.cfg.Device {
				t.Fatal("remote media source was relabeled")
			}
			before := get(t, r.e, ref)
			var signed serviceorigin.SignedRecord
			if err := json.Unmarshal(raw, &signed); err != nil {
				t.Fatal(err)
			}
			verified, err := r.verifier.Verify(ctx, signed)
			if err != nil {
				t.Fatal(err)
			}
			capture, err := sensorrecord.FromComponent(verified.Component())
			if err != nil {
				t.Fatal(err)
			}
			media, err := capture.Evidence()
			if err != nil || media == nil {
				t.Fatal("missing typed media reference", media, err)
			}
			if kind == "sensorlink" {
				original, _ := capture.Sample()
				if capture.SensorLink == nil || media.ContentType != "image/jpeg" || original.Evidence.ContentType != "image/png" || media.SHA256 == original.Evidence.SHA256 {
					t.Fatal("native JPEG and original PNG ancestry were conflated")
				}
			}
			sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(verified.Component())
			want := verified.Record.Publication.Timing.SampleTime.Add(3 * time.Second)
			if kind == "audio" {
				want = want.Add(-250 * time.Millisecond)
			}
			if err != nil || !sampleUntil.Equal(want) || !physicalUntil.Equal(time.Unix(0, capture.CapturedUnixNS).Add(sensorrecord.MaxPhysicalAge)) {
				t.Fatal("original media interpretation deadlines changed", sampleUntil, physicalUntil, err)
			}
			r.e.clock.advance(100 * time.Millisecond)
			duplicate, err := r.e.a.RememberRemoteAccepted(ctx, raw)
			if err != nil || duplicate != ref || !reflect.DeepEqual(before, get(t, r.e, ref)) {
				t.Fatal("duplicate renewed or rewrote media capture", err)
			}
			claimRaw := r.e.raw(t, true, "remote-media", ref)
			pin, err := r.e.a.Pin(ctx, PinRequest{ClaimEnvelope: claimRaw, Inputs: []Reference{ref}, RetainUntilUnixMS: r.e.until(time.Minute)})
			if err != nil || len(pin.Inputs) != 1 || pin.Inputs[0] != ref {
				t.Fatal("exact foreign capture was not available as a local claim input", err)
			}
			reopen(t, r.e)
			if got := get(t, r.e, ref); !got.Historical || !bytes.Equal(got.Envelope, raw) {
				t.Fatal("reopen changed original remote camera/audio envelope")
			}
			// Retained media references do not make expired observations live.
			r.sampleNow = sampleUntil
			expiredClaim := r.e.raw(t, true, "expired-media", ref)
			if _, err := r.e.a.Pin(ctx, PinRequest{ClaimEnvelope: expiredClaim, Inputs: []Reference{ref}, RetainUntilUnixMS: r.e.until(time.Minute)}); err == nil {
				t.Fatal("historical media envelope extended original observation lifetime")
			}
			if got := get(t, r.e, ref); !bytes.Equal(got.Envelope, raw) {
				t.Fatal("expired historical proof changed")
			}
		})
	}
}

func TestRemoteMalformedMediaEnvelopeCannotReachCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"camera", "audio", "sensorlink"} {
		t.Run(kind, func(t *testing.T) {
			r := remoteSetup(t)
			request := remoteRequest(t, r, kind)
			capture, err := sensorrecord.ValidatePublication(request)
			if err != nil {
				t.Fatal(err)
			}
			if capture.SensorLink != nil {
				capture.SensorLink.CalibrationSHA256 = "invalid"
			} else {
				sample, _ := capture.Sample()
				sample.Evidence.ContentType = "application/octet-stream"
				capture.SampleJSON = encode(sample)
			}
			request.Payload = encode(capture) // Deliberately bypass typed Encode.
			raw := archiveSigned(t, r.e, r.id, r.certs, r.e.cfg.Site, request)
			r.accepted[digest(raw)] = bytes.Clone(raw)
			before := r.calls
			if _, err := r.e.a.RememberRemoteAccepted(context.Background(), raw); !errors.Is(err, ErrDenied) || r.calls != before {
				t.Fatal("malformed remote media capture reached authority or storage", err, r.calls-before)
			}
		})
	}
}
