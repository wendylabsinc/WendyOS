package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func src(id, kind, clock string, healthy bool, detail string) *agentpbv2.DataSource {
	return &agentpbv2.DataSource{Id: id, Kind: kind, ClockDomain: clock, Healthy: healthy, Detail: detail}
}

// orinNanoSources mirrors the real `wendy data sources` answer from a Jetson
// Orin Nano: one USB webcam, one USB microphone, and 20 internal APE ADMAIF
// audio-DMA routing channels that bury everything else.
func orinNanoSources() []*agentpbv2.DataSource {
	sources := []*agentpbv2.DataSource{
		src("applications", "application", "CLOCK_BOOTTIME", true, ""),
		src("audio:16777217", "audio", "ALSA_CAPTURE/AGENT_RECEIPT", true, "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio] plughw:0,0"),
	}
	for i := 0; i < 20; i++ {
		sources = append(sources, src(
			fmt.Sprintf("audio:%d", 16777729+i), "audio", "ALSA_CAPTURE/AGENT_RECEIPT", true,
			fmt.Sprintf("APE [NVIDIA Jetson Orin Nano APE], device %d: fe.admaif@290f000.ADMAIF%d (*) [] plughw:2,%d", i, i+1, i)))
	}
	return append(sources,
		src("telemetry", "telemetry", "CLOCK_BOOTTIME", true, ""),
		src("v4l2:/dev/video0", "camera", "V4L2_BUFFER_TIMESTAMP", true, "HD Pro Webcam C920: HD Pro Webc VIDEO_TRANSPORT_USB"))
}

func renderSources(t *testing.T, sources []*agentpbv2.DataSource, kinds ...string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeDataSources(&buf, sources, normalizeSourceKinds(kinds)); err != nil {
		t.Fatalf("writeDataSources: %v", err)
	}
	return buf.String()
}

func manyOfKind(kind string, n int) []*agentpbv2.DataSource {
	var out []*agentpbv2.DataSource
	for i := 0; i < n; i++ {
		out = append(out, src(fmt.Sprintf("%s:%d", kind, i), kind, "CLOCK_BOOTTIME", true, ""))
	}
	return out
}

func TestWriteDataSourcesTable(t *testing.T) {
	cases := []struct {
		name        string
		sources     []*agentpbv2.DataSource
		kinds       []string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "aligned header without detail column",
			sources:     []*agentpbv2.DataSource{src("applications", "application", "CLOCK_BOOTTIME", true, ""), src("telemetry", "telemetry", "CLOCK_BOOTTIME", false, "")},
			wantContain: []string{"SOURCE        KIND         CLOCK           STATUS", "applications  application  CLOCK_BOOTTIME  healthy", "telemetry     telemetry    CLOCK_BOOTTIME  unhealthy"},
			wantAbsent:  []string{"DETAIL", "\t"},
		},
		{
			name:        "detail column present and aligned",
			sources:     []*agentpbv2.DataSource{src("applications", "application", "CLOCK_BOOTTIME", true, ""), src("v4l2:/dev/video0", "camera", "V4L2_BUFFER_TIMESTAMP", true, "HD Pro Webcam C920")},
			wantContain: []string{"STATUS   DETAIL", "v4l2:/dev/video0  camera       V4L2_BUFFER_TIMESTAMP  healthy  HD Pro Webcam C920"},
			wantAbsent:  []string{"\t"},
		},
		{
			name:        "kind filter keeps only the asked-for kind",
			sources:     orinNanoSources(),
			kinds:       []string{"camera"},
			wantContain: []string{"v4l2:/dev/video0"},
			wantAbsent:  []string{"applications", "telemetry", "audio:"},
		},
		{
			name:        "kind filter is case insensitive",
			sources:     orinNanoSources(),
			kinds:       []string{"CaMeRa"},
			wantContain: []string{"v4l2:/dev/video0"},
			wantAbsent:  []string{"audio:"},
		},
		{
			name:        "multiple kinds",
			sources:     orinNanoSources(),
			kinds:       []string{"camera", "telemetry"},
			wantContain: []string{"v4l2:/dev/video0", "telemetry"},
			wantAbsent:  []string{"audio:", "applications"},
		},
		{
			name:        "unknown kind names the kinds present",
			sources:     orinNanoSources(),
			kinds:       []string{"lidar"},
			wantContain: []string{`No sources of kind "lidar".`, "Kinds present on this device: application, audio, telemetry, camera."},
			wantAbsent:  []string{"SOURCE", "audio:"},
		},
		{
			name:        "empty device result",
			sources:     nil,
			wantContain: []string{"No recordable sources reported by the device."},
			wantAbsent:  []string{"SOURCE"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderSources(t, c.sources, c.kinds...)
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Fatalf("output unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}
}

func TestWriteDataSourcesDetailTruncation(t *testing.T) {
	long := "APE [NVIDIA Jetson Orin Nano APE], device 19: fe.admaif@290f000.ADMAIF20 (*) [] plughw:2,19"
	got := renderSources(t, []*agentpbv2.DataSource{src("audio:1", "audio", "ALSA_CAPTURE", true, long)})
	line := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "audio:1") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no source row in output:\n%s", got)
	}
	detail := strings.TrimSpace(line[strings.Index(line, "healthy")+len("healthy"):])
	if len([]rune(detail)) != maxSourceDetailWidth {
		t.Fatalf("detail width = %d, want %d: %q", len([]rune(detail)), maxSourceDetailWidth, detail)
	}
	if !strings.HasSuffix(detail, "...") {
		t.Fatalf("truncated detail should end in three dots: %q", detail)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(detail, "...")) {
		t.Fatalf("truncated detail %q is not a prefix of %q", detail, long)
	}

	short := "HD Pro Webcam C920"
	if got := truncateSourceDetail(short); got != short {
		t.Fatalf("short detail was altered: %q", got)
	}
	exact := strings.Repeat("x", maxSourceDetailWidth)
	if got := truncateSourceDetail(exact); got != exact {
		t.Fatalf("detail of exactly the max width was truncated: %q", got)
	}
}

func TestWriteDataSourcesDetailColumnOmittedWhenAllEmpty(t *testing.T) {
	got := renderSources(t, []*agentpbv2.DataSource{
		src("applications", "application", "CLOCK_BOOTTIME", true, ""),
		src("telemetry", "telemetry", "CLOCK_BOOTTIME", true, ""),
	})
	if strings.Contains(got, "DETAIL") {
		t.Fatalf("DETAIL column shown although no source carries a detail:\n%s", got)
	}
	if !strings.HasSuffix(strings.Split(got, "\n")[0], "STATUS") {
		t.Fatalf("header should end at STATUS:\n%s", got)
	}
}

func TestWriteDataSourcesFloodBoundary(t *testing.T) {
	cases := []struct {
		name        string
		count       int
		kinds       []string
		wantRows    int
		wantContain []string
		wantAbsent  []string
	}{
		{name: "exactly the limit lists every row", count: sourceKindFloodLimit, wantRows: sourceKindFloodLimit, wantAbsent: []string{"more audio"}},
		{
			name:        "one over the limit summarises the remainder",
			count:       sourceKindFloodLimit + 1,
			wantRows:    sourceKindFloodLimit,
			wantContain: []string{"... 1 more audio source not listed (--kind audio to list all, --json for everything)"},
		},
		{
			name:        "jetson audio flood",
			count:       21,
			wantRows:    sourceKindFloodLimit,
			wantContain: []string{"... 15 more audio sources not listed (--kind audio to list all, --json for everything)"},
		},
		{
			name:       "explicitly requested kind is never summarised",
			count:      21,
			kinds:      []string{"audio"},
			wantRows:   21,
			wantAbsent: []string{"more audio"},
		},
		{
			name:       "explicit request is case insensitive too",
			count:      21,
			kinds:      []string{"AUDIO"},
			wantRows:   21,
			wantAbsent: []string{"more audio"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderSources(t, manyOfKind("audio", c.count), c.kinds...)
			rows := 0
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(line, "audio:") {
					rows++
				}
			}
			if rows != c.wantRows {
				t.Fatalf("listed %d rows, want %d:\n%s", rows, c.wantRows, got)
			}
			for _, want := range c.wantContain {
				if !strings.Contains(got, want) {
					t.Fatalf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(got, absent) {
					t.Fatalf("output unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}
}

// Summarisation must never hide a kind entirely, and it must not disturb the
// kinds that are not flooding the table.
func TestWriteDataSourcesFloodKeepsOtherKinds(t *testing.T) {
	got := renderSources(t, orinNanoSources())
	for _, want := range []string{"applications", "telemetry", "v4l2:/dev/video0", "audio:16777217", "... 15 more audio sources not listed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\t") {
		t.Fatalf("output still contains raw tabs:\n%s", got)
	}
}

// --json without --kind must stay byte-for-byte the full proto encoding,
// because scripts consume it.
func TestDataSourcesJSONUnchangedWithoutKind(t *testing.T) {
	response := &agentpbv2.DataSourcesResponse{Sources: orinNanoSources()}

	var want bytes.Buffer
	if err := json.NewEncoder(&want).Encode(response); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got bytes.Buffer
	if err := encodeDataSourcesJSON(&got, response, normalizeSourceKinds(nil)); err != nil {
		t.Fatalf("encodeDataSourcesJSON: %v", err)
	}
	if got.String() != want.String() {
		t.Fatalf("json output changed\n got: %s\nwant: %s", got.String(), want.String())
	}
	if !strings.Contains(got.String(), "audio:16777748") {
		t.Fatalf("json output was summarised; it must carry every source:\n%s", got.String())
	}

	var filtered bytes.Buffer
	if err := encodeDataSourcesJSON(&filtered, response, normalizeSourceKinds([]string{"camera"})); err != nil {
		t.Fatalf("encodeDataSourcesJSON: %v", err)
	}
	if strings.Contains(filtered.String(), "audio:") {
		t.Fatalf("--kind camera --json should drop audio sources:\n%s", filtered.String())
	}
	if !strings.Contains(filtered.String(), "v4l2:/dev/video0") {
		t.Fatalf("--kind camera --json dropped the camera:\n%s", filtered.String())
	}
}

func TestNormalizeSourceKinds(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil", in: nil, want: nil},
		{name: "lowercases and trims", in: []string{" Camera ", "AUDIO"}, want: []string{"camera", "audio"}},
		{name: "drops empties from comma splits", in: []string{"camera", "", "  "}, want: []string{"camera"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSourceKinds(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("normalizeSourceKinds(%q) = %q, want %q", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("normalizeSourceKinds(%q) = %q, want %q", c.in, got, c.want)
				}
			}
		})
	}
}
