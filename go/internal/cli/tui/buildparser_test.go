package tui

import (
	"os"
	"strings"
	"testing"
	"time"
)

// collect runs the given plain-progress text through the parser and returns the
// emitted events.
func collect(t *testing.T, text string) []BuildStepEvent {
	t.Helper()
	var got []BuildStepEvent
	p := NewBuildParser(func(e BuildStepEvent) { got = append(got, e) })
	if _, err := p.Write([]byte(text)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return got
}

func TestParserEmitsStepStartAndDoneWithDuration(t *testing.T) {
	text := "#9 [4/6] RUN pip install -r requirements.txt\n" +
		"#9 1.563 Collecting debugpy\n" +
		"#9 DONE 4.3s\n"
	got := collect(t, text)
	// start, one progress update from the log line, done.
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(got), got)
	}
	if got[0].Status != BuildStepRunning || got[0].Kind != BuildVertexStep ||
		got[0].Display != "[4/6] RUN pip install -r requirements.txt" {
		t.Errorf("start event wrong: %+v", got[0])
	}
	if got[1].Status != BuildStepRunning || got[1].Detail != "collecting debugpy" {
		t.Errorf("progress event wrong: %+v", got[1])
	}
	last := got[len(got)-1]
	if last.Status != BuildStepDone || last.Dur != 4300*time.Millisecond {
		t.Errorf("done event wrong: %+v (dur=%v)", last, last.Dur)
	}
}

func TestParserMarksCachedStep(t *testing.T) {
	text := "#6 [1/6] FROM docker.io/library/python:3.11-slim\n#6 CACHED\n"
	got := collect(t, text)
	if len(got) != 2 || got[1].Status != BuildStepCached {
		t.Fatalf("want a cached event, got %+v", got)
	}
}

func TestParserHidesInternalNoise(t *testing.T) {
	// .dockerignore / cache manifest / resolve are noise. Build context is not
	// in this list any more: transferring a multi-gigabyte context is one of the
	// slowest things a build does, so it gets a visible row (see
	// TestParserShowsBuildContextTransferProgress).
	text := "#3 [internal] load .dockerignore\n#3 DONE 0.0s\n" +
		"#4 importing cache manifest from local:123\n#4 DONE 0.0s\n"
	if got := collect(t, text); len(got) != 0 {
		t.Fatalf("want no events for internal noise, got %+v", got)
	}
}

func TestParserShowsSetupSteps(t *testing.T) {
	text := "#1 [internal] load build definition from Dockerfile\n#1 DONE 0.0s\n" +
		"#2 [internal] load metadata for docker.io/library/python:3.11-slim\n#2 DONE 2.0s\n"
	got := collect(t, text)
	if len(got) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != BuildVertexSetup || got[0].Display != "load build definition" {
		t.Errorf("event 0 wrong: %+v", got[0])
	}
	if got[2].Display != "load metadata" {
		t.Errorf("event 2 wrong: %+v", got[2])
	}
}

func TestParserCollapsesExportAndPushIntoOnePhase(t *testing.T) {
	text := "#12 exporting to image\n#12 exporting layers\n#12 DONE 0.5s\n" +
		"#13 exporting cache to client directory\n#13 DONE 0.3s\n" +
		"#12 pushing layers\n#12 DONE 12.6s\n"
	got := collect(t, text)
	// Exactly one Running export event, no per-vertex done/cached events.
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed export event, got %d: %+v", len(got), got)
	}
	if got[0].ID != BuildExportVertexID || got[0].Kind != BuildVertexExport ||
		got[0].Status != BuildStepRunning || got[0].Display != "exporting + pushing layers" {
		t.Errorf("export event wrong: %+v", got[0])
	}
}

func TestParserMarksFailedStep(t *testing.T) {
	text := "#9 [4/6] RUN pip install -r requirements.txt\n" +
		"#9 ERROR: process \"/bin/sh -c pip install\" did not complete successfully\n"
	got := collect(t, text)
	if len(got) != 2 || got[1].Status != BuildStepFailed {
		t.Fatalf("want a failed event, got %+v", got)
	}
}

func TestParseBuildContextTransferBytes(t *testing.T) {
	cases := []struct {
		line string
		want int64
	}{
		{"#4 transferring context: 2B", 2},
		{"#4 transferring context: 2B done", 2},
		{"#4 transferring context: 40B", 40},
		{"#4 transferring context: 1.5kB", 1500},
		{"#4 transferring context: 1.0 MiB", 1024 * 1024},
	}
	for _, tc := range cases {
		got, ok := ParseBuildContextTransferBytes(tc.line)
		if !ok || got != tc.want {
			t.Fatalf("ParseBuildContextTransferBytes(%q) = %d, %v; want %d, true", tc.line, got, ok, tc.want)
		}
	}
	if _, ok := ParseBuildContextTransferBytes("#4 DONE 0.0s"); ok {
		t.Fatal("unexpected context transfer match")
	}
}

func TestParserHandlesSplitWrites(t *testing.T) {
	var got []BuildStepEvent
	p := NewBuildParser(func(e BuildStepEvent) { got = append(got, e) })
	p.Write([]byte("#9 [4/6] RUN pip insta"))
	p.Write([]byte("ll\n#9 DON"))
	p.Write([]byte("E 4.3s\n"))
	if len(got) != 2 || got[0].Status != BuildStepRunning || got[1].Status != BuildStepDone {
		t.Fatalf("split writes mis-parsed: %+v", got)
	}
}

func TestParserHidesInternalVertexWithFraction(t *testing.T) {
	text := "#7 [internal 2/3] settle layers\n#7 DONE 0.0s\n"
	if got := collect(t, text); len(got) != 0 {
		t.Fatalf("want no events for an [internal] vertex with N/N fraction, got %+v", got)
	}
}

// Transferring a large build context is one of the slowest, least explained
// parts of a build, so it gets a visible row carrying its byte counters.
func TestParserShowsBuildContextTransferProgress(t *testing.T) {
	text := "#5 [internal] load build context\n" +
		"#5 transferring context: 1.20MB / 45.60MB\n" +
		"#5 DONE 3.0s\n"
	got := collect(t, text)
	if len(got) < 2 {
		t.Fatalf("want build context events, got %+v", got)
	}
	if got[0].Kind != BuildVertexSetup || got[0].Display != "load build context" {
		t.Fatalf("start event wrong: %+v", got[0])
	}
	var sawBytes bool
	for _, e := range got {
		if e.Bytes.Current == 1_200_000 && e.Bytes.Total == 45_600_000 {
			sawBytes = true
		}
	}
	if !sawBytes {
		t.Fatalf("want context transfer byte counters, got %+v", got)
	}
}

// A base-image pull is invisible in the old view; it is often the longest phase
// of a cold build.
func TestParserSurfacesBaseImagePullWithBytes(t *testing.T) {
	text := "#5 docker-image://nvcr.io/nvidia/l4t-base:r36.2@sha256:abc\n" +
		"#5 resolve nvcr.io/nvidia/l4t-base:r36.2 done\n" +
		"#5 sha256:layer 5.24MB / 27.09MB 1.2s\n"
	got := collect(t, text)
	if got[0].Kind != BuildVertexPull || got[0].Display != "pull nvidia/l4t-base:r36.2" {
		t.Fatalf("pull event wrong: %+v", got[0])
	}
	last := got[len(got)-1]
	if last.Bytes.Current != 5_240_000 || last.Bytes.Total != 27_090_000 {
		t.Fatalf("want pull byte counters, got %+v", last.Bytes)
	}
}

// eventsFor returns the events for the row whose display starts with prefix.
func eventsFor(got []BuildStepEvent, prefix string) []BuildStepEvent {
	var out []BuildStepEvent
	for _, e := range got {
		if strings.HasPrefix(e.Display, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// collectFile replays a recorded progress stream with throttling disabled.
func collectFile(t *testing.T, path string) []BuildStepEvent {
	t.Helper()
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return collectRaw(t, string(fixture))
}

// TestParserReopensLazilyPulledBaseImage replays BuildKit's plain printer (the
// build host's `buildctl`, which streams --progress=plain) over a lazily pulled
// 6.5 GB base image: testdata/buildkit-lazy-pull.jsonl replayed at its recorded
// pace through progressui.NewDisplay(PlainMode) from buildkit v0.32.2. The FROM
// reports DONE after 2s, then its layers are pulled and unpacked under the same
// "#1", which used to leave the row frozen on "✓ … 2s" for nine minutes.
func TestParserReopensLazilyPulledBaseImage(t *testing.T) {
	got := collectFile(t, "testdata/buildkit-lazy-pull-plain.txt")
	from := eventsFor(got, "[base-nvidia-jetson 1/5] FROM")
	if len(from) < 4 {
		t.Fatalf("want the FROM to start, finish, reopen and finish, got %+v", from)
	}
	if from[1].Status != BuildStepDone || from[1].Dur != 2*time.Second {
		t.Fatalf("first DONE wrong: %+v", from[1])
	}
	if reopen := from[2]; reopen.Status != BuildStepRunning || reopen.Bytes.Total != 3_400_000_000 {
		t.Fatalf("want the first layer download to reopen the step, got %+v", reopen)
	}

	var sawBytes bool
	var details []string
	for _, e := range from[2 : len(from)-1] {
		if e.Status != BuildStepRunning {
			t.Fatalf("reopened step finished early: %+v", e)
		}
		if e.Bytes.Current == 900_000_000 && e.Bytes.Total == 3_400_000_000 {
			sawBytes = true
		}
		if e.Detail != "" && (len(details) == 0 || details[len(details)-1] != e.Detail) {
			details = append(details, e.Detail)
		}
	}
	if !sawBytes {
		t.Errorf("want mid-pull byte counters, got %+v", from)
	}
	wantDetails := []string{
		"unpacking layer 1", "unpacked 1 layer",
		"unpacking layer 2", "unpacked 2 layers",
		"unpacking layer 3", "unpacked 3 layers",
	}
	if strings.Join(details, "|") != strings.Join(wantDetails, "|") {
		t.Errorf("unpack details = %q, want %q", details, wantDetails)
	}

	last := from[len(from)-1]
	if last.Status != BuildStepDone || last.Dur != 533600*time.Millisecond {
		t.Fatalf("final DONE wrong: %+v (dur=%v)", last, last.Dur)
	}
	if run := eventsFor(got, "[base-nvidia-jetson 2/5] RUN"); run[len(run)-1].Status != BuildStepDone {
		t.Fatalf("RUN step should finish, got %+v", run)
	}
}

// TestParserReopensBaseImageOnRealBuildxPlainOutput is the everyday shape of the
// same thing, a small alpine pull: BuildKit's plain printer replaying the real
// buildx recording. Setup steps also report after their first DONE, but only
// unsized bookkeeping, and must not reopen.
func TestParserReopensBaseImageOnRealBuildxPlainOutput(t *testing.T) {
	got := collectFile(t, "testdata/buildx-plain.txt")
	from := eventsFor(got, "[1/2] FROM")
	if len(from) < 3 || from[1].Status != BuildStepDone || from[2].Status != BuildStepRunning {
		t.Fatalf("want DONE then a reopen, got %+v", from)
	}
	var unpacking bool
	for _, e := range from {
		if e.Detail == "unpacking layer 1" && e.Bytes.Total == 4_090_000 {
			unpacking = true
		}
	}
	if !unpacking {
		t.Errorf("want an unpacking detail beside the layer download, got %+v", from)
	}
	if last := from[len(from)-1]; last.Status != BuildStepDone || last.Dur != 500*time.Millisecond {
		t.Fatalf("final DONE wrong: %+v", last)
	}

	def := eventsFor(got, "load build definition")
	if len(def) != 2 || def[0].Status != BuildStepRunning || def[1].Status != BuildStepDone {
		t.Fatalf("setup step should not reopen, got %+v", def)
	}
}

// Only work in progress reopens a step. The tail of a finished run ("… done"
// lines) and anything after a failure leave the row alone, and export vertices
// stay collapsed.
func TestParserDoesNotReopenWithoutNewWork(t *testing.T) {
	cases := map[string]string{
		"finished transfer": "#5 [1/2] FROM docker.io/library/alpine:3.20\n#5 DONE 0.1s\n" +
			"#5 sha256:3f26bc2dec0b 4.09MB / 4.09MB 0.3s done\n" +
			"#5 extracting sha256:3f26bc2dec0b 0.1s done\n",
		"failed step": "#9 [4/6] RUN pip install\n#9 ERROR: process did not complete successfully\n" +
			"#9 sha256:3f26bc2dec0b 1.00MB / 4.09MB 0.3s\n" +
			"#9 extracting sha256:3f26bc2dec0b\n",
		"export": "#12 exporting to image\n#12 DONE 0.5s\n" +
			"#12 sha256:3f26bc2dec0b 1.00MB / 4.09MB 0.3s\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got := collectRaw(t, text)
			for _, e := range got[1:] {
				if e.Status == BuildStepRunning {
					t.Fatalf("unexpected reopen: %+v", got)
				}
			}
		})
	}
}

// The reopen must reach the renderer even inside the detail throttle window, or
// the row stays "done" until some later update happens to get through.
func TestParserReopenIsNotThrottled(t *testing.T) {
	var got []BuildStepEvent
	p := NewBuildParser(func(e BuildStepEvent) { got = append(got, e) })
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return at }
	p.Write([]byte("#5 [1/2] FROM docker.io/library/alpine:3.20\n" +
		"#5 sha256:aaaa 1.00MB / 2.00MB 0.1s\n" +
		"#5 DONE 0.2s\n" +
		"#5 sha256:bbbb 1.00MB / 4.09MB 0.3s\n"))
	last := got[len(got)-1]
	if last.Status != BuildStepRunning || last.Bytes.Total != 4_090_000 {
		t.Fatalf("reopen was throttled away: %+v", got)
	}
}
