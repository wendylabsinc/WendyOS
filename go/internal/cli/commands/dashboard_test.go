package commands

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// ansiRE strips SGR color codes (and any other escape sequences) so tests can
// assert on the visible content of a rendered row.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func strBody(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func strAttr(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: strBody(val)}
}

func TestSanitizeLogText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"unicode format controls dropped", "a\u202e\u2066b\u200bc\u200d\ufeff\nd", "abc\nd"},
		{"carriage return dropped", "abc\rdef", "abcdef"},
		{"cursor erase escape removed", "pulling manifest ⠋\x1b[2K", "pulling manifest ⠋"},
		{"cursor up escape removed", "x\x1b[1Ay", "xy"},
		{"sgr color removed", "\x1b[31mred\x1b[0m", "red"},
		{"tab becomes space", "a\tb", "a b"},
		{"nul and bell dropped", "a\x00\x07b", "ab"},
		{"newline preserved", "line1\nline2", "line1\nline2"},
		{"spinner braille preserved", "⠋⠙⠹", "⠋⠙⠹"},
		{"osc title with bel terminator dropped", "\x1b]0;evil title\x07after", "after"},
		{"osc title with st terminator dropped", "\x1b]0;evil title\x1b\\after", "after"},
		{"dcs sequence dropped", "\x1bPq#payload\x1b\\done", "done"},
		{"8-bit csi dropped", "before\x9b2Kafter", "beforeafter"},
		{"two-char escape does not swallow following text", "x\x1b7y", "xy"},
		{"two-char keypad escape dropped", "a\x1b=b", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLogText(tc.in); got != tc.want {
				t.Fatalf("sanitizeLogText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// noRowContainsControlBytes asserts the layout-breaking bytes never survive into
// a rendered row. Raw newline/carriage-return and cursor-movement/erase escapes
// would corrupt the BubbleTea stdout grid.
func noRowContainsControlBytes(t *testing.T, rows []string) {
	t.Helper()
	for i, row := range rows {
		if strings.ContainsAny(row, "\n\r") {
			t.Fatalf("row %d contains raw newline/carriage return: %q", i, row)
		}
		// After stripping SGR color codes, no escape byte should remain.
		if strings.ContainsRune(stripANSI(row), '\x1b') {
			t.Fatalf("row %d contains a non-color escape sequence: %q", i, row)
		}
	}
}

func TestFormatLogLinesMultilineSplitsIntoRows(t *testing.T) {
	lr := &logspb.LogRecord{Body: strBody("line1\nline2\nline3")}
	rows := formatLogLines("svc", lr)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %#v", len(rows), rows)
	}
	noRowContainsControlBytes(t, rows)

	if !strings.HasSuffix(stripANSI(rows[0]), "line1") {
		t.Errorf("row 0 should end with body line1, got %q", stripANSI(rows[0]))
	}
	// Continuation rows are indented under the prefix and carry only body text.
	for i, want := range []string{"line2", "line3"} {
		row := stripANSI(rows[i+1])
		if !strings.HasPrefix(row, " ") {
			t.Errorf("continuation row %d should be indented, got %q", i+1, row)
		}
		if strings.TrimLeft(row, " ") != want {
			t.Errorf("continuation row %d = %q, want indented %q", i+1, row, want)
		}
	}
}

func TestFormatLogLinesStripsProgressControlSequences(t *testing.T) {
	// A spinner line as produced by `ollama pull`: trailing erase + carriage return.
	lr := &logspb.LogRecord{Body: strBody("pulling manifest ⠋\x1b[2K\r")}
	rows := formatLogLines("llm-app", lr)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row (trailing empty dropped), got %d: %#v", len(rows), rows)
	}
	noRowContainsControlBytes(t, rows)
	if !strings.Contains(stripANSI(rows[0]), "pulling manifest ⠋") {
		t.Errorf("row should retain visible body, got %q", stripANSI(rows[0]))
	}
}

func TestFormatLogLinesAttributesOnLastRow(t *testing.T) {
	lr := &logspb.LogRecord{
		Body:       strBody("a\nb"),
		Attributes: []*commonpb.KeyValue{strAttr("stream", "stderr")},
	}
	rows := formatLogLines("svc", lr)

	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %#v", len(rows), rows)
	}
	noRowContainsControlBytes(t, rows)
	last := stripANSI(rows[len(rows)-1])
	if !strings.Contains(last, "stream=stderr") {
		t.Errorf("attributes should be appended to the last row, got %q", last)
	}
	if strings.Contains(stripANSI(rows[0]), "stream=stderr") {
		t.Errorf("attributes should not appear on the first row, got %q", stripANSI(rows[0]))
	}
}

func TestFormatLogLinesPreservesInteriorBlankLines(t *testing.T) {
	// A body with an intentional interior blank line and a single terminating
	// newline: the interior blank is kept, only the terminator empty is dropped.
	lr := &logspb.LogRecord{Body: strBody("a\n\nb\n")}
	rows := formatLogLines("svc", lr)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (interior blank kept, terminator dropped), got %d: %#v", len(rows), rows)
	}
	noRowContainsControlBytes(t, rows)
	if got := strings.TrimSpace(stripANSI(rows[1])); got != "" {
		t.Errorf("row 1 should be the preserved blank line, got %q", got)
	}
	if !strings.HasSuffix(stripANSI(rows[2]), "b") {
		t.Errorf("row 2 should carry body line b, got %q", stripANSI(rows[2]))
	}
}

func TestDashboardLogsHorizontalScrollCropsAtOffset(t *testing.T) {
	m := dashboardModel{
		width:  60,
		height: 12,
		logs:   []string{"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
	}

	// At offset 0 the visible row begins at the start of the line.
	rows := m.visibleLogRows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 visible row, got %d: %#v", len(rows), rows)
	}
	if got := stripANSI(rows[0]); !strings.HasPrefix(got, "0123") {
		t.Fatalf("offset 0 should start at the line start, got %q", got)
	}

	// Panning right shifts the visible window further into the line, exposing
	// content that was previously off-screen to the right.
	m.logHOffset = 10
	rows = m.visibleLogRows()
	if got := stripANSI(rows[0]); !strings.HasPrefix(got, "ABC") {
		t.Fatalf("offset 10 should start at 'A', got %q", got)
	}
}

func TestDashboardLogsHorizontalScrollRightClampsToWidestLine(t *testing.T) {
	m := dashboardModel{
		width:  60,
		height: 12,
		logs:   []string{"short", strings.Repeat("x", 100)},
	}

	var model tea.Model = m
	for range 50 { // far more presses than needed to reach the clamp
		model, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	dm := model.(dashboardModel)

	if dm.logHOffset == 0 {
		t.Fatalf("expected a non-zero horizontal offset after panning right")
	}
	if maxOff := dm.maxLogHOffset(); dm.logHOffset != maxOff {
		t.Fatalf("expected horizontal offset clamped to %d, got %d", maxOff, dm.logHOffset)
	}
}

func TestDashboardLogsHorizontalScrollLeftClampsAtZero(t *testing.T) {
	m := dashboardModel{
		width:      60,
		height:     12,
		logs:       []string{strings.Repeat("x", 100)},
		logHOffset: 5,
	}

	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	dm := model.(dashboardModel)
	if dm.logHOffset != 0 {
		t.Fatalf("expected horizontal offset clamped to 0, got %d", dm.logHOffset)
	}
}

func TestFormatLogLinesNilBody(t *testing.T) {
	lr := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{strAttr("k", "v")}}
	rows := formatLogLines("svc", lr)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for body-less record, got %d: %#v", len(rows), rows)
	}
	noRowContainsControlBytes(t, rows)
	if !strings.Contains(stripANSI(rows[0]), "k=v") {
		t.Errorf("expected attributes on the single row, got %q", stripANSI(rows[0]))
	}
}
