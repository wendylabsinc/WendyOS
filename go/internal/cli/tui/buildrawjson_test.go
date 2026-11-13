package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestParser is NewBuildParser with detail throttling disabled: tests replay
// a whole build in microseconds, and the 80ms production throttle would collapse
// progress updates that are seconds apart in the recording.
func newTestParser(emit func(BuildStepEvent)) *BuildParser {
	p := NewBuildParser(emit)
	p.throttle = 0
	return p
}

func collectRaw(t *testing.T, text string) []BuildStepEvent {
	t.Helper()
	var got []BuildStepEvent
	p := newTestParser(func(e BuildStepEvent) { got = append(got, e) })
	if _, err := p.Write([]byte(text)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return got
}

// TestRawJSONParserDecodesRealBuildxOutput replays a recording of
// `docker buildx build --progress rawjson` (buildx v0.33.0) captured from a real
// build whose RUN step prints "[N/5] Compiling ThingN.swift".
func TestRawJSONParserDecodesRealBuildxOutput(t *testing.T) {
	fixture, err := os.ReadFile("testdata/buildx-rawjson.jsonl")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	got := collectRaw(t, string(fixture))
	if len(got) == 0 {
		t.Fatal("no events decoded from rawjson fixture")
	}

	var details []string
	byDisplay := map[string][]BuildStepEvent{}
	for _, e := range got {
		byDisplay[e.Display] = append(byDisplay[e.Display], e)
		if e.Detail != "" {
			details = append(details, e.Detail)
		}
	}

	// The compiler-style counter in the RUN step's own stdout is surfaced.
	wantDetail := "[5/5] 100%  Compiling Thing5.swift"
	found := false
	for _, d := range details {
		if d == wantDetail {
			found = true
		}
	}
	if !found {
		t.Errorf("want a %q detail, got %q", wantDetail, details)
	}

	// Internal noise stays hidden, real steps appear and complete.
	if _, ok := byDisplay["[internal] load .dockerignore"]; ok {
		t.Error("load .dockerignore should be hidden")
	}
	runStep := ""
	for d := range byDisplay {
		if strings.HasPrefix(d, "[2/2] RUN") {
			runStep = d
		}
	}
	if runStep == "" {
		t.Fatalf("no RUN step decoded; displays=%v", keysOf(byDisplay))
	}
	evs := byDisplay[runStep]
	if evs[0].Status != BuildStepRunning {
		t.Errorf("RUN step should start Running, got %v", evs[0].Status)
	}
	last := evs[len(evs)-1]
	if last.Status != BuildStepDone || last.Dur <= 0 {
		t.Errorf("RUN step should finish Done with a duration, got %+v", last)
	}
	// Vertices are keyed by digest on this path, not by "#N".
	if !strings.HasPrefix(last.ID, "sha256:") {
		t.Errorf("want a digest ID, got %q", last.ID)
	}

	// Every exporting/pushing vertex collapses into the one synthetic phase.
	for _, e := range got {
		if e.Kind == BuildVertexExport && e.ID != BuildExportVertexID {
			t.Errorf("export vertex not collapsed: %+v", e)
		}
	}
}

func keysOf(m map[string][]BuildStepEvent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRawJSONParserSumsByteCountersAndDerivesRate(t *testing.T) {
	const dg = "sha256:ffff"
	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	frames := []string{
		fmt.Sprintf(`{"vertexes":[{"digest":%q,"name":"docker-image://nvcr.io/nvidia/l4t-base:r36.2","started":%q}]}`,
			dg, start.Format(time.RFC3339Nano)),
		// Two layers of one pull, four seconds in.
		fmt.Sprintf(`{"statuses":[{"id":"layer-a","vertex":%q,"name":"downloading","current":3000000,"total":10000000,"started":%q,"timestamp":%q}]}`,
			dg, start.Format(time.RFC3339Nano), start.Add(4*time.Second).Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"statuses":[{"id":"layer-b","vertex":%q,"name":"downloading","current":1000000,"total":10000000,"started":%q,"timestamp":%q}]}`,
			dg, start.Format(time.RFC3339Nano), start.Add(4*time.Second).Format(time.RFC3339Nano)),
		// A counter with no total is a completion marker, not a transfer; it must
		// not drag the percentage around.
		fmt.Sprintf(`{"statuses":[{"id":"marker","vertex":%q,"name":"done","current":42,"timestamp":%q}]}`,
			dg, start.Add(4*time.Second).Format(time.RFC3339Nano)),
	}
	got := collectRaw(t, strings.Join(frames, "\n")+"\n")

	last := got[len(got)-1]
	if last.Bytes.Current != 4000000 || last.Bytes.Total != 20000000 {
		t.Fatalf("bytes = %+v, want current=4000000 total=20000000", last.Bytes)
	}
	// 4 MB over 4 seconds.
	if last.Bytes.Rate < 990000 || last.Bytes.Rate > 1010000 {
		t.Errorf("rate = %v, want ~1e6 bytes/sec", last.Bytes.Rate)
	}
	if s := last.Bytes.String(); s != "20%  4.0MB/20.0MB  1.0MB/s" {
		t.Errorf("rendered = %q", s)
	}
	if last.Display != "pull nvidia/l4t-base:r36.2" {
		t.Errorf("display = %q, want the shortened image ref", last.Display)
	}
}

func TestRawJSONParserReassemblesLogLinesSplitAcrossFrames(t *testing.T) {
	const dg = "sha256:aaaa"
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	frames := []string{
		fmt.Sprintf(`{"vertexes":[{"digest":%q,"name":"[2/3] RUN swift build -c release","started":"2026-08-09T12:00:00Z"}]}`, dg),
		fmt.Sprintf(`{"logs":[{"vertex":%q,"stream":1,"data":%q}]}`, dg, enc("[525/10")),
		fmt.Sprintf(`{"logs":[{"vertex":%q,"stream":1,"data":%q}]}`, dg, enc("27] Compiling WendyKit\n")),
	}
	got := collectRaw(t, strings.Join(frames, "\n")+"\n")
	last := got[len(got)-1]
	if last.Detail != "[525/1027] 51%  Compiling WendyKit" {
		t.Fatalf("detail = %q", last.Detail)
	}
}

// TestParserAcceptsMixedRawJSONAndPlainLines covers buildx warnings, which are
// printed as bare text on the same stream as the JSON frames.
func TestParserAcceptsMixedRawJSONAndPlainLines(t *testing.T) {
	text := `WARNING: buildx: something happened
{"vertexes":[{"digest":"sha256:bbbb","name":"[1/2] RUN make","started":"2026-08-09T12:00:00Z"}]}
{"not":"a frame"}
#9 [4/6] RUN pip install
#9 DONE 1.0s
`
	got := collectRaw(t, text)
	var haveDigest, havePlain bool
	for _, e := range got {
		if e.ID == "sha256:bbbb" {
			haveDigest = true
		}
		if e.ID == "#9" {
			havePlain = true
		}
	}
	if !haveDigest || !havePlain {
		t.Fatalf("want both a rawjson and a plain-text vertex, got %+v", got)
	}
}

func TestRawJSONParserMarksCachedAndFailedVertices(t *testing.T) {
	text := `{"vertexes":[{"digest":"sha256:c1","name":"[1/2] RUN a","started":"2026-08-09T12:00:00Z"}]}
{"vertexes":[{"digest":"sha256:c1","name":"[1/2] RUN a","cached":true,"started":"2026-08-09T12:00:00Z","completed":"2026-08-09T12:00:00Z"}]}
{"vertexes":[{"digest":"sha256:e1","name":"[2/2] RUN b","started":"2026-08-09T12:00:00Z"}]}
{"vertexes":[{"digest":"sha256:e1","name":"[2/2] RUN b","error":"exit code 1","started":"2026-08-09T12:00:00Z","completed":"2026-08-09T12:00:02Z"}]}
`
	var cached, failed bool
	for _, e := range collectRaw(t, text) {
		switch e.Status {
		case BuildStepCached:
			cached = true
		case BuildStepFailed:
			failed = true
		}
	}
	if !cached || !failed {
		t.Fatalf("want a cached and a failed event (cached=%v failed=%v)", cached, failed)
	}
}

// TestRawJSONParserReopensLazilyPulledBaseImage replays a lazily pulled 6.5 GB
// base image in BuildKit's own rawjson frames, shaped on buildkit v0.32.2: the
// FROM's records come from the puller's progress controller, which reports a
// fresh run each time it picks up work, so the vertex finishes after its 2s
// resolve, runs again for the layer downloads, finishes between two unpacks,
// and runs once more. testdata/buildkit-lazy-pull-plain.txt is the same stream
// through BuildKit's plain printer.
func TestRawJSONParserReopensLazilyPulledBaseImage(t *testing.T) {
	got := collectFile(t, "testdata/buildkit-lazy-pull.jsonl")
	from := eventsFor(got, "[base-nvidia-jetson 1/5] FROM")

	var statuses []BuildStepStatus
	var details []string
	var maxBytes ByteProgress
	for _, e := range from {
		if len(statuses) == 0 || statuses[len(statuses)-1] != e.Status {
			statuses = append(statuses, e.Status)
		}
		if e.Detail != "" && (len(details) == 0 || details[len(details)-1] != e.Detail) {
			details = append(details, e.Detail)
		}
		if e.Bytes.Current > maxBytes.Current {
			maxBytes = e.Bytes
		}
	}
	want := []BuildStepStatus{
		BuildStepRunning, BuildStepDone, // cache check; stays done through the resolve
		BuildStepRunning, BuildStepDone, // layer downloads and the first unpack
		BuildStepRunning, BuildStepDone, // the remaining unpacks
	}
	if fmt.Sprint(statuses) != fmt.Sprint(want) {
		t.Fatalf("FROM statuses = %v, want %v", statuses, want)
	}
	wantDetails := []string{
		"unpacking layer 1", "unpacked 1 layer",
		"unpacking layer 2", "unpacked 2 layers",
		"unpacking layer 3", "unpacked 3 layers",
	}
	if strings.Join(details, "|") != strings.Join(wantDetails, "|") {
		t.Errorf("unpack details = %q, want %q", details, wantDetails)
	}
	// Every layer of the pull sums into one figure.
	if maxBytes.Current != 6_500_007_400 || maxBytes.Total != 6_500_007_400 {
		t.Errorf("peak bytes = %+v, want the whole 6.5 GB pull", maxBytes)
	}

	// The final duration covers every run, overlaps once, and agrees with the
	// "DONE 533.6s" BuildKit's plain printer gives the same stream.
	last := from[len(from)-1]
	if got := last.Dur.Round(100 * time.Millisecond); got != 533600*time.Millisecond {
		t.Errorf("final duration = %v, want 533.6s", last.Dur)
	}
}

// TestRawJSONParserReopensRealBuildxBaseImagePull: in the real recording the
// alpine FROM finishes before its 4 MB layer is fetched and unpacked. Setup
// steps that send unsized bookkeeping after their first finish stay finished.
func TestRawJSONParserReopensRealBuildxBaseImagePull(t *testing.T) {
	got := collectFile(t, "testdata/buildx-rawjson.jsonl")
	from := eventsFor(got, "[1/2] FROM")
	if len(from) < 3 || from[1].Status != BuildStepDone || from[2].Status != BuildStepRunning {
		t.Fatalf("want DONE then a reopen, got %+v", from)
	}
	var unpacking bool
	for _, e := range from {
		if e.Detail == "unpacking layer 1" && e.Bytes.Total == 4_103_199 {
			unpacking = true
		}
	}
	if !unpacking {
		t.Errorf("want an unpacking detail beside the summed pull, got %+v", from)
	}
	// BuildKit's plain printer says "DONE 0.5s" for this vertex.
	last := from[len(from)-1]
	if last.Status != BuildStepDone || last.Dur.Round(100*time.Millisecond) != 500*time.Millisecond {
		t.Fatalf("final DONE wrong: %+v (dur=%v)", last, last.Dur)
	}

	def := eventsFor(got, "load build definition")
	if len(def) != 2 || def[0].Status != BuildStepRunning || def[1].Status != BuildStepDone {
		t.Fatalf("setup step should not reopen, got %+v", def)
	}
}

// A status can land in the same frame as, or after, the vertex's completion
// while belonging to the run that just ended. Only an open status written after
// the vertex finished is new work, and a failed vertex never reopens.
func TestRawJSONParserDoesNotReopenOnStaleOrFailedStatus(t *testing.T) {
	cases := map[string]string{
		"finished in the same frame": `{"vertexes":[{"digest":"sha256:f1","name":"[1/2] FROM docker.io/library/alpine:3.20","started":"2026-10-27T09:00:00Z"}]}
{"vertexes":[{"digest":"sha256:f1","name":"[1/2] FROM docker.io/library/alpine:3.20","started":"2026-10-27T09:00:00Z","completed":"2026-10-27T09:00:02Z"}],"statuses":[{"id":"sha256:aa","vertex":"sha256:f1","current":4090000,"total":4090000,"timestamp":"2026-10-27T09:00:02Z","started":"2026-10-27T09:00:00Z","completed":"2026-10-27T09:00:02Z"},{"id":"extracting sha256:aa","vertex":"sha256:f1","name":"extracting","current":0,"timestamp":"2026-10-27T09:00:02Z","started":"2026-10-27T09:00:01Z","completed":"2026-10-27T09:00:02Z"}]}
`,
		"open but written before the finish": `{"vertexes":[{"digest":"sha256:f1","name":"[1/2] FROM docker.io/library/alpine:3.20","started":"2026-10-27T09:00:00Z","completed":"2026-10-27T09:00:02Z"}]}
{"statuses":[{"id":"sha256:aa","vertex":"sha256:f1","current":1000000,"total":4090000,"timestamp":"2026-10-27T09:00:01Z","started":"2026-10-27T09:00:00Z"}]}
`,
		"failed": `{"vertexes":[{"digest":"sha256:e1","name":"[2/2] RUN make","started":"2026-10-27T09:00:00Z","completed":"2026-10-27T09:00:02Z","error":"exit code 1"}]}
{"statuses":[{"id":"sha256:aa","vertex":"sha256:e1","current":1000000,"total":4090000,"timestamp":"2026-10-27T09:00:05Z","started":"2026-10-27T09:00:04Z"}]}
`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got := collectRaw(t, text)
			for i, e := range got {
				if i > 0 && e.Status == BuildStepRunning {
					t.Fatalf("unexpected reopen: %+v", got)
				}
			}
		})
	}
}

// TestRawJSONParserAgainstRealPipAndAptOutput replays a recording of a real
// build that runs `apt-get install`, `pip install numpy`, and a compiler-style
// counter. It is the guard against the sniffers being written from a
// remembered output format rather than the one these tools actually print.
func TestRawJSONParserAgainstRealPipAndAptOutput(t *testing.T) {
	fixture, err := os.ReadFile("testdata/buildx-rawjson-pip-apt.jsonl")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	// Details seen per step, in order.
	seen := map[string][]string{}
	p := newTestParser(func(e BuildStepEvent) {
		if e.Status == BuildStepRunning && e.Detail != "" {
			seen[e.Display] = append(seen[e.Display], e.Detail)
		}
	})
	if _, err := p.Write(fixture); err != nil {
		t.Fatalf("Write: %v", err)
	}

	find := func(prefix string) []string {
		for d, v := range seen {
			if strings.Contains(d, prefix) {
				return v
			}
		}
		return nil
	}
	has := func(details []string, want string) bool {
		for _, d := range details {
			if d == want {
				return true
			}
		}
		return false
	}

	apt := find("apt-get")
	for _, want := range []string{
		"fetching trixie-updates InRelease [47.3 kB]",
		"fetching trixie/main arm64 Packages [9607 kB]",
		"fetched 9940 kB (8645 kB/s)",
	} {
		if !has(apt, want) {
			t.Errorf("apt step missing detail %q\ngot: %q", want, apt)
		}
	}

	pip := find("pip install")
	for _, want := range []string{
		"collecting numpy",
		"100%  15.6/15.6 MB  95.2 MB/s",
	} {
		if !has(pip, want) {
			t.Errorf("pip step missing detail %q\ngot: %q", want, pip)
		}
	}
	// pip's trailing "[notice] A new release of pip is available" must not be
	// mistaken for progress.
	for _, d := range pip {
		if strings.Contains(d, "new release of pip") {
			t.Errorf("pip notice leaked into progress: %q", d)
		}
	}

	counter := find("Compiling Thing")
	if !has(counter, "[3/3] 100%  Compiling Thing3.swift") {
		t.Errorf("counter step missing final detail\ngot: %q", counter)
	}
}
