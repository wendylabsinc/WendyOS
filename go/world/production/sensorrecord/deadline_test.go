package sensorrecord

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLiveDeadlinesUseOriginalTimeAndWholeCaptureInterval(t *testing.T) {
	for _, kind := range []string{"gas", "audio", "camera"} {
		t.Run(kind, func(t *testing.T) {
			c := testComponent(testPublication(t, kind))
			c.Timing.ErrorBound = 100 * time.Millisecond
			c.Timing.AgeHint = 500 * time.Millisecond
			want := c.Timing.SampleTime.Add(1500 * time.Millisecond)
			if kind == "audio" {
				want = want.Add(-250 * time.Millisecond)
			}
			if kind == "camera" {
				capture, _ := Decode(c.Payload)
				sample, _ := capture.Sample()
				sample.Camera.CalibrationValidForNS = int64(time.Second)
				capture.SampleJSON, _ = json.Marshal(sample)
				c.Payload, _ = Encode(capture)
				want = c.Timing.SampleTime.Add(500 * time.Millisecond)
			}
			until, physical, err := LiveDeadlines(c)
			if err != nil || !until.Equal(want) || !physical.Equal(testWall.Add(MaxPhysicalAge)) {
				t.Fatal("incorrect capture bounds", until, physical, err)
			}
			again, _, err := LiveDeadlines(c)
			if err != nil || !again.Equal(until) {
				t.Fatal("repeated lookup changed lifetime", err)
			}
		})
	}
}

func TestLiveDeadlinesRefuseExhaustedAudioCalibrationAndAge(t *testing.T) {
	for _, kind := range []string{"gas", "audio", "camera"} {
		t.Run(kind, func(t *testing.T) {
			c := testComponent(testPublication(t, kind))
			if kind == "audio" {
				c.TTL = 250 * time.Millisecond
			} else if kind == "gas" {
				c.Timing.AgeHint = c.TTL
			} else {
				capture, _ := Decode(c.Payload)
				sample, _ := capture.Sample()
				sample.Camera.CalibrationValidForNS = int64(time.Second)
				capture.SampleJSON, _ = json.Marshal(sample)
				c.Payload, _ = Encode(capture)
				c.Timing.AgeHint = time.Second
			}
			if _, _, err := LiveDeadlines(c); err == nil {
				t.Fatal("expired interpretation accepted")
			}
		})
	}
}
