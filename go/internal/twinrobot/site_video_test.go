package twinrobot

import (
	"bytes"
	"context"
	"testing"
)

func TestSiteVideoDropsDependentPicturesUntilKeyframeAfterOverflow(t *testing.T) {
	frames := make(chan []byte, 2)
	waiting := false
	keyframe := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 1, 0x68, 0x80, 0, 0, 1, 0x65, 0x80}
	inter := []byte{0, 0, 1, 0x41, 0x80}
	queueSiteVideo(frames, keyframe, &waiting)
	queueSiteVideo(frames, inter, &waiting)
	queueSiteVideo(frames, inter, &waiting)
	queueSiteVideo(frames, inter, &waiting)
	if len(frames) != 0 || !waiting {
		t.Fatal("overflow continued a GOP after dropping a reference picture")
	}
	queueSiteVideo(frames, keyframe, &waiting)
	queueSiteVideo(frames, inter, &waiting)
	r := &SiteRobot{ctx: context.Background(), videoFrames: frames}
	got, err := r.NextVideo()
	want := append(bytes.Clone(keyframe), inter...)
	if err != nil || !bytes.Equal(got, want) || waiting {
		t.Fatalf("recovered batch = %x, %v; want %x", got, err, want)
	}
	if len(frames) != 0 {
		t.Fatal("one HTTP read did not drain the contiguous video backlog")
	}
}

func TestSiteVideoOverflowCanResumeWithTheArrivingKeyframe(t *testing.T) {
	frames := make(chan []byte, 2)
	waiting := false
	inter := []byte{0, 0, 1, 0x41, 0x80}
	keyframe := []byte{0, 0, 1, 0x65, 0x80}
	queueSiteVideo(frames, inter, &waiting)
	queueSiteVideo(frames, inter, &waiting)
	queueSiteVideo(frames, keyframe, &waiting)
	if len(frames) != 1 || waiting || !bytes.Equal(<-frames, keyframe) {
		t.Fatal("new keyframe was lost behind obsolete video backlog")
	}
}

func TestSiteVideoBatchReturnsFinalQueuedPicturesWhenStreamEnds(t *testing.T) {
	frames := make(chan []byte, 8)
	frames <- []byte{0, 0, 1, 0x65, 0x80}
	frames <- []byte{0, 0, 1, 0x41, 0x80}
	close(frames)
	r := &SiteRobot{ctx: context.Background(), videoFrames: frames}
	got, err := r.NextVideo()
	if err != nil || len(got) != 10 {
		t.Fatalf("final batch = %x, %v", got, err)
	}
	if _, err := r.NextVideo(); err == nil {
		t.Fatal("closed stream returned an empty successful frame")
	}
}
